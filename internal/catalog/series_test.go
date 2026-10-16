package catalog

import (
	"testing"

	"coinapp-backend/internal/ecb"
)

// A national 2 euro coin and a commemorative coin can share a country, a year
// and even a fingerprint. Letting the matcher see across the two series would
// hand one the other's id and silently repoint a user's saved count at a
// different coin, so the two must never resolve to the same id.
func TestNationalDoesNotAdoptACommemorativeId(t *testing.T) {
	existing := map[string]Doc{
		"comm-1": {
			ID: "comm-1", Country: "Italy", Slug: "italy", Year: 2015,
			Feature: "National side, 2015 design", FP: "same-fp", Kind: KindCommemorative,
		},
	}
	idx := buildIndexes(existing, nil)

	incoming := Doc{
		ID: "", Country: "Italy", Slug: "italy", Year: 2015,
		Feature: "National side, 2015 design", FP: "same-fp", Kind: KindNational,
	}
	id, how, _ := resolve(incoming, idx, map[string]struct{}{})
	if id != "" || how != "new" {
		t.Fatalf("national coin matched the commemorative id %q (%s); it must be new", id, how)
	}
}

// The converse: a national coin must still match the national coin stored by the
// previous run, or every sync would mint fresh ids and orphan the counts.
func TestNationalKeepsItsIdAcrossRuns(t *testing.T) {
	existing := map[string]Doc{
		"nat-1": {
			ID: "nat-1", Country: "Italy", Slug: "italy", Year: 2015,
			Feature: "National side, 2015 design", FP: "same-fp", Kind: KindNational,
		},
	}
	idx := buildIndexes(existing, nil)

	incoming := Doc{
		Country: "Italy", Slug: "italy", Year: 2015,
		Feature: "National side, 2015 design", FP: "same-fp", Kind: KindNational,
	}
	id, how, _ := resolve(incoming, idx, map[string]struct{}{})
	if id != "nat-1" {
		t.Fatalf("expected the stored national id nat-1, got %q (%s)", id, how)
	}
	if how != "fingerprint" {
		t.Errorf("expected a fingerprint match, got %s", how)
	}
}

// A national design is often a near-copy of a commemorative's wording, and the
// fuzzy matcher has to stay inside the series as well.
func TestNationalDoesNotFuzzyMatchAcrossSeries(t *testing.T) {
	existing := map[string]Doc{
		"comm-1": {
			ID: "comm-1", Country: "Belgium", Slug: "belgium", Year: 2014,
			Feature: "National side, 2014 design", FP: "different-fp", Kind: KindCommemorative,
		},
	}
	idx := buildIndexes(existing, nil)

	incoming := Doc{
		Country: "Belgium", Slug: "belgium", Year: 2014,
		Feature: "National side, 2014 design", FP: "other-fp", Kind: KindNational,
	}
	id, how, _ := resolve(incoming, idx, map[string]struct{}{})
	if id != "" || how != "new" {
		t.Fatalf("fuzzy match crossed series into %q (%s)", id, how)
	}
}

// Documents written before the split have no Kind, and must keep resolving
// against other such documents rather than being treated as a third series.
func TestDocumentsWithoutKindResolveTogether(t *testing.T) {
	existing := map[string]Doc{
		"old-1": {
			ID: "old-1", Country: "Slovenia", Slug: "slovenia", Year: 2025,
			Feature: "Miki Muster", FP: "fp-1",
		},
	}
	idx := buildIndexes(existing, nil)

	incoming := Doc{
		Country: "Slovenia", Slug: "slovenia", Year: 2025,
		Feature: "Miki Muster", FP: "fp-1", Kind: KindCommemorative,
	}
	if id, _, _ := resolve(incoming, idx, map[string]struct{}{}); id != "old-1" {
		t.Fatalf("a Kind-less document should match a commemorative, got %q", id)
	}
}

// A real end-to-end pass: two scrapes of the same national coin must produce the
// same id the second time, and the image URL change must be an in-place update.
func TestNationalIsStableAndUpdatedInPlace(t *testing.T) {
	scraped := []ecb.Coin{
		{
			Country: "France", Feature: "National side, 2022 design", Kind: ecb.KindNational,
			Date: "2022", ImageURLs: []string{"https://example/broken/France_2euro_2022.jpg"},
		},
	}

	first, report := Build(scraped, map[string]Doc{}, nil)
	if len(report.Added) != 1 {
		t.Fatalf("expected one added coin, got %d", len(report.Added))
	}
	firstID := report.Added[0].ID

	// The next scrape has the corrected image URL.
	fixed := []ecb.Coin{
		{
			Country: "France", Feature: "National side, 2022 design", Kind: ecb.KindNational,
			Date: "2022", ImageURLs: []string{"https://example/right/France_2euro_2022.jpg"},
		},
	}
	second, report2 := Build(fixed, first, nil)

	if len(report2.Added) != 0 {
		t.Fatalf("second run added %d coins; ids must be stable", len(report2.Added))
	}
	if len(report2.Removed) != 0 {
		t.Fatalf("second run removed %d coins; a stable coin must not be tombstoned", len(report2.Removed))
	}
	if len(report2.Updated) != 1 {
		t.Fatalf("expected exactly one updated coin, got %d", len(report2.Updated))
	}
	if report2.Updated[0].ID != firstID {
		t.Errorf("id changed between runs: %s -> %s", firstID, report2.Updated[0].ID)
	}
	if got := second[firstID].ImageURL; got == nil || *got != "https://example/right/France_2euro_2022.jpg" {
		t.Errorf("image URL was not corrected in place: %v", got)
	}
}
