// Package derive holds the pure helpers: normalisation, the derived sort fields
// the Android app used to scrape out of prose on the client, and the identity
// fingerprint plus similarity metric that decide whether two scraped coins are
// the same coin.
//
// Nothing here touches the network or Firebase, so all of it is directly testable.
package derive

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

var months = map[string]int{
	"january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
	"july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12,
}

var (
	// Ordinal words mapped to the first month of the span they name.
	quarterOrdinals = []struct {
		re    *regexp.Regexp
		month int
	}{
		{regexp.MustCompile(`\b(first|1st)\b`), 1},
		{regexp.MustCompile(`\b(second|2nd)\b`), 4},
		{regexp.MustCompile(`\b(third|3rd)\b`), 7},
		{regexp.MustCompile(`\b(fourth|4th|last)\b`), 10},
	}

	// "first half" starts in January, "second half" in July.
	halfOrdinals = []struct {
		re    *regexp.Regexp
		month int
	}{
		{regexp.MustCompile(`\b(first|1st)\b`), 1},
		{regexp.MustCompile(`\b(second|2nd)\b`), 7},
	}

	seasons = []struct {
		re    *regexp.Regexp
		month int
	}{
		{regexp.MustCompile(`\bspring\b`), 3},
		{regexp.MustCompile(`\bsummer\b`), 6},
		{regexp.MustCompile(`\b(autumn|fall)\b`), 9},
		{regexp.MustCompile(`\bwinter\b`), 12},
	}

	quarterWord = regexp.MustCompile(`\bquarter\b`)
	halfWord    = regexp.MustCompile(`\bhalf\b`)
	word        = regexp.MustCompile(`[a-z]+`)
	yearDigits  = regexp.MustCompile(`20\d\d`)
	nonDigit    = regexp.MustCompile(`\D`)
	nonAlnum    = regexp.MustCompile(`[^a-z0-9\s]+`)
	spaceRun    = regexp.MustCompile(`[[:space:]]+`)

	nonSlug  = regexp.MustCompile(`[^a-z0-9]+`)
	edgeDash = regexp.MustCompile(`^-+|-+$`)
)

// Normalize collapses all whitespace and lowercases. The ECB pads its labels with
// &nbsp; entities, which are folded to plain spaces first.
func Normalize(v string) string {
	return strings.TrimSpace(spaceRun.ReplaceAllString(strings.ToLower(v), " "))
}

// Slugify produces a lowercase, dash-separated, RTDB-key-safe token.
func Slugify(v string) string {
	s := edgeDash.ReplaceAllString(nonSlug.ReplaceAllString(Normalize(v), "-"), "")
	return s
}

// ParseYear pulls the last 20xx out of free text, e.g. "Fourth quarter of 2025".
func ParseYear(text string) (int, bool) {
	m := yearDigits.FindString(text)
	if m == "" {
		return 0, false
	}
	n := 0
	for _, r := range m {
		n = n*10 + int(r-'0')
	}
	return n, true
}

// ParseSortDate turns the ECB's free-form issuing date into a sortable integer
// (YYYYMMDD).
//
// The ECB uses roughly two dozen shapes: "November 2025", "Fourth quarter of
// 2025", "Autumn 2023", "1 July 2022", "3rd quarter 2021", "2nd half 2020",
// "April-May 2022", "May/June 2018", "Spring/Summer 2020", "Second/third quarter
// of 2023" and "January 2012 for all euro area countries except San Marino
// (May 2012)".
//
// Ranges resolve to their EARLIEST month so "sort by date, newest first" is
// deterministic and never reorders between runs.
func ParseSortDate(text string) (int, bool) {
	year, ok := ParseYear(text)
	if !ok {
		return 0, false
	}

	hay := Normalize(text)

	// Try the categories in specificity order; within one take the earliest month.
	categories := []struct {
		noun  *regexp.Regexp
		table []struct {
			re    *regexp.Regexp
			month int
		}
	}{
		{quarterWord, quarterOrdinals},
		{halfWord, halfOrdinals},
		{nil, seasons},
	}

	for _, category := range categories {
		if category.noun != nil && !category.noun.MatchString(hay) {
			continue
		}
		best := 0
		for _, ordinal := range category.table {
			if ordinal.re.MatchString(hay) && (best == 0 || ordinal.month < best) {
				best = ordinal.month
			}
		}
		if best != 0 {
			return year*10000 + best*100 + 1, true
		}
	}

	best := 0
	for _, w := range word.FindAllString(hay, -1) {
		if m, ok := months[w]; ok && (best == 0 || m < best) {
			best = m
		}
	}
	if best != 0 {
		return year*10000 + best*100 + 1, true
	}

	return year*10000 + 100, true
}

// ParseSortVolume reads "1 000 000 coins" as 1000000, and returns 0 when the ECB
// has not announced a figure yet.
func ParseSortVolume(text string) int64 {
	only := nonDigit.ReplaceAllString(text, "")
	if only == "" {
		return 0
	}
	n, err := strconv.ParseInt(only, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Fingerprint is the stable identity of a coin.
//
// It deliberately covers ONLY country + year + feature. Date, volume,
// description, image URLs and copyright are all excluded because the ECB rewrites
// them in place: it has already changed "Fourth quarter of 2025" to a firm month,
// swapped "placeholder_coming_soon (2).jpg" for a real render, and retyped a
// feature to add a space inside its parentheses. Hashing those would re-key the
// coin and silently move a user's saved coins onto a different coin.
func Fingerprint(country string, year int, feature string) string {
	input := Normalize(country) + "|" + strconv.Itoa(year) + "|" + Normalize(feature)
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

func tokenSet(text string) map[string]struct{} {
	cleaned := nonAlnum.ReplaceAllString(Normalize(text), " ")
	set := map[string]struct{}{}
	for _, token := range strings.Fields(cleaned) {
		if len(token) > 1 {
			set[token] = struct{}{}
		}
	}
	return set
}

// Similarity is the Jaccard overlap of the two feature word sets, in [0, 1].
//
// This is what absorbs ECB micro-edits. "Remembrance cornflower (Bleuet de
// France)" and "Remembrance cornflower ( Bleuet de France )" have identical token
// sets, so they score 1 and are recognised as the same coin. Punctuation is
// dropped, so en dash vs hyphen vs apostrophe never breaks a match either.
func Similarity(a, b string) float64 {
	left, right := tokenSet(a), tokenSet(b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	shared := 0
	for token := range left {
		if _, ok := right[token]; ok {
			shared++
		}
	}
	union := len(left) + len(right) - shared
	return float64(shared) / float64(union)
}
