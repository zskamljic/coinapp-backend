package main

import (
	"encoding/json"

	"testing"
)

func TestMintedIDsLookLikeTheOnesAlreadyInUse(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 2000; i++ {
		id := mintID()
		if !hexID.MatchString(id) {
			t.Fatalf("minted %q, want 16 lowercase hex chars", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("minted a duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNumericIDDetection(t *testing.T) {
	// The legacy ids are array indexes, and "0" is a real one.
	for _, id := range []string{"0", "1", "42", "501"} {
		if !isLegacyID(id) {
			t.Errorf("%q should be treated as legacy", id)
		}
	}
	for _, id := range []string{"148fc368db6d8fe7", "a", "", "1a", "01x", "0x10"} {
		if isLegacyID(id) {
			t.Errorf("%q should not be treated as legacy", id)
		}
	}
}

// A minted id is 16 hex characters, and roughly one in five hundred comes out as
// all digits. A plain ^[0-9]+$ test reads that as a legacy index, which would put
// the coin through the migration twice and detach its count. This is not
// hypothetical: 5973505629001386 is one of the ids this migration minted.
func TestAllDigitMintedIDIsNotLegacy(t *testing.T) {
	const minted = "5973505629001386"
	if !hexID.MatchString(minted) {
		t.Fatalf("%q is not a valid minted id", minted)
	}
	if !decimalID.MatchString(minted) {
		t.Fatalf("%q should match the decimal test, or this proves nothing", minted)
	}
	if isLegacyID(minted) {
		t.Errorf("%q is a minted id that looks numeric; it must not be re-keyed", minted)
	}
}

func TestDecodeCoinMapRejectsTheArrayShape(t *testing.T) {
	if _, err := decodeCoinMap([]byte(`[{"country":"Slovenia"}]`)); err == nil {
		t.Fatal("expected an error for a positional array catalog")
	}
}

func TestDecodeCoinMap(t *testing.T) {
	got, err := decodeCoinMap([]byte(`{"0":{"id":"0"},"ab":{"id":"ab"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 coins, got %d", len(got))
	}
}

// The document's own id field has to move with the key. A coin stored under a
// hex key but still saying id "0" would be a catalog that disagrees with itself,
// and the client uses the id field.
func TestRekeyingRewritesTheDocumentID(t *testing.T) {
	doc := json.RawMessage(`{"id":"42","country":"France","kind":"commemorative"}`)
	var fields map[string]any
	if err := json.Unmarshal(doc, &fields); err != nil {
		t.Fatal(err)
	}
	fields["id"] = "deadbeefdeadbeef"

	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back["id"] != "deadbeefdeadbeef" {
		t.Errorf("id field is %v, want the new hex id", back["id"])
	}
	if back["country"] != "France" {
		t.Errorf("the rest of the document must survive, got %v", back)
	}
}

func TestMappingRoundTrips(t *testing.T) {
	m := map[string]string{"0": "aaaaaaaaaaaaaaaa", "501": "bbbbbbbbbbbbbbbb"}
	path := t.TempDir() + "/id-migration.json"
	if err := writeMapping(path, m); err != nil {
		t.Fatal(err)
	}
	back, err := readMapping(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(m) {
		t.Fatalf("read %d entries, wrote %d", len(back), len(m))
	}
	for k, v := range m {
		if back[k] != v {
			t.Errorf("%s -> %q, want %q", k, back[k], v)
		}
	}
}

// A missing mapping file is not an error: the first run creates one.
func TestReadMappingMissingIsNotFatal(t *testing.T) {
	if _, err := readMapping(t.TempDir() + "/nope.json"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
