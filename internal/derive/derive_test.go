package derive

import "testing"

func TestNormalize(t *testing.T) {
	if got := Normalize("  Remembrance\n Cornflower "); got != "remembrance cornflower" {
		t.Fatalf("got %q", got)
	}
}

func TestSlugify(t *testing.T) {
	for input, want := range map[string]string{
		"Vatican City":        "vatican-city",
		"Euro area countries": "euro-area-countries",
		"San Marino":          "san-marino",
	} {
		if got := Slugify(input); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseYear(t *testing.T) {
	if got, ok := ParseYear("Fourth quarter of 2025"); !ok || got != 2025 {
		t.Fatalf("got %d %v", got, ok)
	}
	if _, ok := ParseYear("no date here"); ok {
		t.Fatal("expected failure")
	}
}

func TestParseSortDate(t *testing.T) {
	cases := map[string]int{
		"November 2025":                20251101,
		"January 2024":                 20240101,
		"Fourth quarter of 2025":       20251001,
		"fourth quarter of 2015":       20151001,
		"Last quarter of 2024":         20241001,
		"3rd quarter 2021":             20210701,
		"2nd half 2020":                20200701,
		"Autumn 2023":                  20230901,
		"Spring 2024":                  20240301,
		"Summer 2021":                  20210601,
		"1 July 2022":                  20220701,
		"25 January 2022":              20220101,
		"April-May 2022":               20220401,
		"September-December 2020":      20200901,
		"May/June 2018":                20180501,
		"Spring/Summer 2020":           20200301,
		"Second/third quarter of 2023": 20230401,
		"October/November 2024":        20241001,
		"January 2012 for all euro area countries except San Marino (May 2012)": 20120101,
	}

	for input, want := range cases {
		got, ok := ParseSortDate(input)
		if !ok || got != want {
			t.Errorf("ParseSortDate(%q) = %d (%v), want %d", input, got, ok, want)
		}
	}

	if _, ok := ParseSortDate("sometime"); ok {
		t.Fatal("expected failure for dateless input")
	}
}

func TestParseSortDateIsMonotonic(t *testing.T) {
	ordered := []string{
		"January 2020", "March 2020", "Second quarter 2020",
		"August 2020", "Fourth quarter 2020", "December 2020",
	}
	previous := 0
	for _, text := range ordered {
		got, _ := ParseSortDate(text)
		if got <= previous {
			t.Fatalf("%q produced %d, not after %d", text, got, previous)
		}
		previous = got
	}
}

func TestParseSortVolume(t *testing.T) {
	for input, want := range map[string]int64{
		"1 000 000 coins": 1000000,
		"20 000 coins":    20000,
		"500000 coins":    500000,
		"not announced":   0,
	} {
		if got := ParseSortVolume(input); got != want {
			t.Errorf("ParseSortVolume(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint("Slovenia", 2025, "The 100th anniversary of birth of Miki Muster")
	b := Fingerprint("slovenia", 2025, "the 100th anniversary of birth of miki muster")
	if a != b {
		t.Fatal("fingerprint should ignore case")
	}

	if Fingerprint("France", 2019, "Remembrance cornflower") == a {
		t.Error("year must affect the fingerprint")
	}
	if Fingerprint("Belgium", 2025, "The 100th anniversary of birth of Miki Muster") == a {
		t.Error("country must affect the fingerprint")
	}
	if len(a) != 16 {
		t.Errorf("expected 16 hex chars, got %d", len(a))
	}
}

func TestSimilarity(t *testing.T) {
	if got := Similarity("Remembrance cornflower (Bleuet de France)", "Remembrance cornflower ( Bleuet de France )"); got != 1 {
		t.Errorf("whitespace edit should score 1, got %v", got)
	}

	if got := Similarity("Lithuanian Ethnographic Regions – Suvalkija", "Lithuanian Ethnographic Regions - Suvalkija"); got != 1 {
		t.Errorf("en dash vs hyphen should score 1, got %v", got)
	}

	if got := Similarity("Olympic Games Paris 2024", "100 years since the French Revolution"); got >= 0.2 {
		t.Errorf("unrelated coins should score low, got %v", got)
	}

	if got := Similarity("25 years of German unity", "35 years of German unity"); got <= 0.4 {
		t.Errorf("partial rewrite should retain some score, got %v", got)
	}

	if got := Similarity("", "anything"); got != 0 {
		t.Errorf("empty input should score 0, got %v", got)
	}
}
