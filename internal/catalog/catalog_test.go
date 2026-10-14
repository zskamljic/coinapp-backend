package catalog

import (
	"encoding/json"
	"os"
	"testing"

	"coinapp-backend/internal/ecb"
)

func coin(over map[string]any) ecb.Coin {
	base := map[string]any{
		"country":     "Slovenia",
		"feature":     "The 100th anniversary of birth of Miki Muster",
		"description": "Three heroes",
		"volume":      "1 000 000 coins",
		"date":        "November 2025",
		"imageUrls": []string{
			"https://www.ecb.europa.eu/euro/coins/comm/html/comm_2025/Slovenia.webp",
			"https://www.ecb.europa.eu/euro/coins/comm/html/comm_2025/Slovenia.jpg",
		},
	}
	for k, v := range over {
		base[k] = v
	}

	out := ecb.Coin{
		Country:     base["country"].(string),
		Feature:     base["feature"].(string),
		Description: base["description"].(string),
		Volume:      base["volume"].(string),
		Date:        base["date"].(string),
	}
	for _, u := range base["imageUrls"].([]string) {
		out.ImageURLs = append(out.ImageURLs, u)
	}
	return out
}

func seed(t *testing.T, coins ...ecb.Coin) map[string]Doc {
	t.Helper()
	out, _ := Build(coins, nil, nil)
	return out
}

func onlyID(t *testing.T, catalog map[string]Doc) string {
	t.Helper()
	if len(catalog) != 1 {
		t.Fatalf("expected exactly one coin, got %d", len(catalog))
	}
	for id := range catalog {
		return id
	}
	return ""
}

func TestToDocumentDerivesSortFields(t *testing.T) {
	doc := ToDocument(coin(nil))
	if doc.Year != 2025 || doc.SortDate != 20251101 || doc.SortVol != 1000000 {
		t.Fatalf("derived fields wrong: %+v", doc)
	}
	if doc.Slug != "slovenia" {
		t.Errorf("slug = %q", doc.Slug)
	}
}

func TestToDocumentPrefersWebP(t *testing.T) {
	doc := ToDocument(coin(nil))
	if doc.ImageURL == nil || !hasSuffix(*doc.ImageURL, ".webp") {
		t.Errorf("primary image = %v", doc.ImageURL)
	}
	if doc.ImageFallback == nil || !hasSuffix(*doc.ImageFallback, ".jpg") {
		t.Errorf("fallback image = %v", doc.ImageFallback)
	}
	if doc.ImageMissing {
		t.Error("should not be flagged missing")
	}
}

func TestToDocumentStripsPlaceholders(t *testing.T) {
	doc := ToDocument(coin(map[string]any{
		"imageUrls": []string{"https://www.ecb.europa.eu/euro/coins/comm/html/comm_2025/placeholder_coming_soon%20(1).webp"},
	}))
	if !doc.ImageMissing {
		t.Error("placeholder should be flagged")
	}
	if doc.ImageURL != nil || doc.ImageFallback != nil {
		t.Errorf("placeholder urls must be nil, got %v / %v", doc.ImageURL, doc.ImageFallback)
	}
}

func TestIDAssignedOnce(t *testing.T) {
	id := onlyID(t, seed(t, coin(nil)))
	if len(id) != 16 {
		t.Fatalf("expected 16 hex chars, got %q", id)
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("id is not hex: %q", id)
		}
	}
}

func TestIDStableWhenNothingChanges(t *testing.T) {
	first := seed(t, coin(nil))
	id := onlyID(t, first)

	second, report := Build([]ecb.Coin{coin(nil)}, first, nil)
	if onlyID(t, second) != id {
		t.Fatal("id changed on an identical run")
	}
	if report.Unchanged != 1 {
		t.Errorf("expected 1 unchanged, got %d", report.Unchanged)
	}
}

func TestIDSurvivesFieldsExcludedFromFingerprint(t *testing.T) {
	first := seed(t, coin(nil))
	id := onlyID(t, first)

	// This is the real-world edit: trailing whitespace, a rewritten description, a
	// corrected volume and a revealed image. None may re-key the coin.
	second, report := Build([]ecb.Coin{coin(map[string]any{
		"feature":     "The 100th anniversary of birth of Miki Muster ",
		"description": "A completely rewritten description.",
		"volume":      "1 500 000 coins",
		"imageUrls":   []string{"https://www.ecb.europa.eu/euro/coins/comm/html/comm_2025/Slovenia.webp"},
	})}, first, nil)

	if onlyID(t, second) != id {
		t.Fatal("id changed on a field edit")
	}
	if len(report.Added) != 0 || len(report.Removed) != 0 {
		t.Errorf("unexpected added/removed: %s", report)
	}
	if second[id].Descr != "A completely rewritten description." {
		t.Error("description was not updated")
	}
	if second[id].SortVol != 1500000 {
		t.Error("volume was not updated")
	}
}

func TestIDSurvivesFeatureRewrite(t *testing.T) {
	first := seed(t, coin(nil))
	id := onlyID(t, first)

	second, report := Build([]ecb.Coin{coin(map[string]any{
		"feature": "100th anniversary of the birth of Miki Muster",
	})}, first, nil)

	if onlyID(t, second) != id {
		t.Fatal("id changed after a feature rewrite")
	}
	if len(report.Rematched) != 1 {
		t.Fatalf("expected 1 rematch, got %d", len(report.Rematched))
	}
	if report.Rematched[0].Score < MatchThreshold {
		t.Errorf("score %v below threshold", report.Rematched[0].Score)
	}
}

func TestNewCoinDoesNotDisturbOthers(t *testing.T) {
	first := seed(t,
		coin(nil),
		coin(map[string]any{"country": "France", "feature": "Notre-Dame"}),
	)

	ids := map[string]string{}
	for _, doc := range first {
		ids[doc.Country] = doc.ID
	}

	second, report := Build([]ecb.Coin{
		coin(map[string]any{"country": "France", "feature": "Notre-Dame"}),
		coin(nil),
		coin(map[string]any{"country": "Latvia", "feature": "Sēlija"}),
	}, first, nil)

	for country, id := range ids {
		if _, ok := second[id]; !ok {
			t.Errorf("existing %s coin lost its id", country)
		}
	}
	if len(report.Added) != 1 || report.Added[0].Feature != "Sēlija" {
		t.Errorf("unexpected additions: %+v", report.Added)
	}
}

func TestDoesNotConfuseSameCountryAndYear(t *testing.T) {
	first := seed(t, coin(map[string]any{
		"feature": "Olympic Games Paris 2024", "date": "January 2023",
	}))
	id := onlyID(t, first)

	second, report := Build([]ecb.Coin{
		coin(map[string]any{"feature": "Olympic Games Paris 2024", "date": "January 2023"}),
		coin(map[string]any{"feature": "100 years since the French Revolution", "date": "September 2023"}),
	}, first, nil)

	if _, ok := second[id]; !ok {
		t.Fatal("original coin lost its id")
	}
	if len(report.Added) != 1 || report.Added[0].Feature != "100 years since the French Revolution" {
		t.Errorf("unexpected additions: %+v", report.Added)
	}
}

func TestRemovedCoinIsTombstonedNotDeleted(t *testing.T) {
	first := seed(t,
		coin(nil),
		coin(map[string]any{"country": "Malta", "feature": "City of Mdina"}),
	)
	var slovenia, malta string
	for _, doc := range first {
		if doc.Slug == "malta" {
			malta = doc.ID
		} else {
			slovenia = doc.ID
		}
	}

	withoutMalta, _ := Build([]ecb.Coin{coin(nil)}, first, nil)
	if !withoutMalta[malta].Removed {
		t.Error("withdrawn coin should be tombstoned")
	}
	if _, ok := withoutMalta[slovenia]; !ok {
		t.Error("unrelated coin was disturbed")
	}

	restored, _ := Build([]ecb.Coin{
		coin(nil),
		coin(map[string]any{"country": "Malta", "feature": "City of Mdina"}),
	}, withoutMalta, nil)

	if restored[malta].Removed {
		t.Error("restored coin should be live again")
	}
	if len(restored) != 2 {
		t.Errorf("expected 2 coins, got %d", len(restored))
	}
}

func TestAlreadyRemovedCoinIsNotReportedAgain(t *testing.T) {
	first := seed(t,
		coin(nil),
		coin(map[string]any{"country": "Malta", "feature": "City of Mdina"}),
	)
	var malta string
	for _, doc := range first {
		if doc.Slug == "malta" {
			malta = doc.ID
		}
	}
	if malta == "" {
		t.Fatal("malta coin missing from the first run")
	}

	withdrawn, withdrawReport := Build([]ecb.Coin{coin(nil)}, first, nil)
	if len(withdrawReport.Removed) != 1 {
		t.Fatalf("a fresh withdrawal should be reported once, got %+v", withdrawReport.Removed)
	}
	if !withdrawn[malta].Removed {
		t.Fatal("withdrawn coin should be tombstoned")
	}

	// The next run sees the same input plus the tombstone. It must be a no-op:
	// re-reporting the tombstone would keep Report.Changed() true forever and
	// make every sync rewrite the entire catalog.
	second, secondReport := Build([]ecb.Coin{coin(nil)}, withdrawn, nil)
	if len(secondReport.Removed) != 0 {
		t.Errorf("the tombstone was reported as withdrawn again: %+v", secondReport.Removed)
	}
	if secondReport.Changed() {
		t.Errorf("a no-op run with a persistent tombstone still reports a change: %s", secondReport)
	}
	if !second[malta].Removed {
		t.Error("the tombstone disappeared")
	}
}

func TestManualOverride(t *testing.T) {
	first := seed(t, coin(nil))
	overrides := map[string]string{"slovenia|2025|renamed by an editor": "pinned-id"}

	second, _ := Build([]ecb.Coin{coin(map[string]any{"feature": "Renamed by an editor"})}, first, overrides)
	if _, ok := second["pinned-id"]; !ok {
		t.Fatalf("override id missing, got %v", SortedIDs(second))
	}
}

func TestIDsStayUniqueWithinARun(t *testing.T) {
	out, _ := Build([]ecb.Coin{
		coin(nil),
		coin(map[string]any{"country": "France", "feature": "Notre-Dame"}),
		coin(map[string]any{"country": "France", "feature": "Notre-Dame"}),
	}, nil, nil)

	seen := map[string]struct{}{}
	for id := range out {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(out) != 3 {
		t.Errorf("expected 3 coins, got %d", len(out))
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// TestReplayAgainstRealSnapshot runs the whole pipeline over the 502 coins the
// Java seeder produced in July 2025, then re-runs it to prove idempotency.
func TestReplayAgainstRealSnapshot(t *testing.T) {
	body, err := os.ReadFile("../../testdata/snapshot-2025-07.json")
	if err != nil {
		t.Fatalf("reading snapshot: %v", err)
	}

	var snapshot []ecb.Coin
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("decoding snapshot: %v", err)
	}
	if len(snapshot) != 502 {
		t.Fatalf("expected 502 coins in snapshot, got %d", len(snapshot))
	}

	first, firstReport := Build(snapshot, nil, nil)
	if len(first) != 502 {
		t.Fatalf("expected 502 catalog entries, got %d", len(first))
	}
	if len(firstReport.Added) != 502 {
		t.Errorf("expected 502 additions, got %d", len(firstReport.Added))
	}

	second, secondReport := Build(snapshot, first, nil)
	if len(secondReport.Added) != 0 {
		t.Errorf("a re-run with identical input added %d coins", len(secondReport.Added))
	}
	if len(secondReport.Removed) != 0 {
		t.Errorf("a re-run with identical input removed %d coins", len(secondReport.Removed))
	}
	if secondReport.Unchanged != 502 {
		t.Errorf("expected 502 unchanged, got %d", secondReport.Unchanged)
	}
	if len(first) != len(second) {
		t.Fatalf("catalog size changed on an identical run: %d -> %d", len(first), len(second))
	}
}

// TestSnapshotSurvivesObservedECBEdits replays the three changes the live site
// shows between July 2025 and today.
func TestSnapshotSurvivesObservedECBEdits(t *testing.T) {
	body, _ := os.ReadFile("../../testdata/snapshot-2025-07.json")
	var snapshot []ecb.Coin
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("decoding snapshot: %v", err)
	}

	first, _ := Build(snapshot, nil, nil)

	byFeature := map[string]string{}
	for _, doc := range first {
		byFeature[doc.Feature] = doc.ID
	}

	const cornflower = "Remembrance cornflower (Bleuet de France)"

	var today []ecb.Coin
	for _, entry := range snapshot {
		if entry.Feature == cornflower {
			continue // the ECB replaced this entry
		}
		today = append(today, entry)
	}
	today = append(today,
		ecb.Coin{Country: "Germany", Feature: "35 years of German unity", Description: "new", Volume: "5 000 000 coins", Date: "September 2025"},
		ecb.Coin{Country: "Latvia", Feature: "Sēlija", Description: "new", Volume: "100 000 coins", Date: "October 2025"},
		ecb.Coin{Country: "France", Feature: "Remembrance cornflower ( Bleuet de France )", Description: "retyped by the ECB", Volume: "2 000 000 coins", Date: "January 2018"},
	)

	second, report := Build(today, first, nil)

	if len(report.Added) != 2 {
		t.Errorf("expected 2 genuinely new coins, got %d: %+v", len(report.Added), report.Added)
	}
	if len(report.Rematched) != 1 {
		t.Errorf("expected 1 rematch, got %d: %+v", len(report.Rematched), report.Rematched)
	}
	if len(report.Removed) != 0 {
		t.Errorf("nothing should have been withdrawn, got %+v", report.Removed)
	}

	franceID, ok := byFeature[cornflower]
	if !ok {
		t.Fatal("cornflower coin missing from the first run")
	}
	if _, ok := second[franceID]; !ok {
		t.Fatal("the edited French coin lost its id, so user counts would be orphaned")
	}
	if second[franceID].Feature != "Remembrance cornflower ( Bleuet de France )" {
		t.Errorf("edited feature not applied: %q", second[franceID].Feature)
	}

	for feature, id := range byFeature {
		if feature == cornflower {
			continue
		}
		if _, ok := second[id]; !ok {
			t.Errorf("id for %q did not survive", feature)
		}
	}
	// 501 untouched snapshot coins + the reworded cornflower (keeping its id)
	// + 2 genuinely new ones.
	if len(second) != 504 {
		t.Errorf("expected 504 catalog entries, got %d", len(second))
	}
}

// TestDuplicateWordingInOneRun documents the pathological case where the ECB
// briefly lists both the old and the new wording.
func TestDuplicateWordingInOneRun(t *testing.T) {
	body, _ := os.ReadFile("../../testdata/snapshot-2025-07.json")
	var snapshot []ecb.Coin
	_ = json.Unmarshal(body, &snapshot)

	first, _ := Build(snapshot, nil, nil)

	var cornflowerID string
	for _, doc := range first {
		if doc.Feature == "Remembrance cornflower (Bleuet de France)" {
			cornflowerID = doc.ID
		}
	}

	doubled := append([]ecb.Coin{}, snapshot...)
	doubled = append(doubled, ecb.Coin{Country: "France", Feature: "Remembrance cornflower ( Bleuet de France )", Date: "January 2018"})

	second, report := Build(doubled, first, nil)

	seen := map[string]struct{}{}
	for id := range second {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(report.Added) != 1 {
		t.Errorf("expected the second wording to be reported as new, got %+v", report.Added)
	}
	if _, ok := second[cornflowerID]; !ok {
		t.Error("the original id was disturbed")
	}
}
