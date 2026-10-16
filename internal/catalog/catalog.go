// Package catalog builds the document set that gets written to Firebase.
//
// The contract this package exists to guarantee:
//
//	A coin's id is assigned once, randomly, and is NEVER changed. Not when a new
//	coin appears earlier in the ECB's year page, not when the ECB rewrites a
//	description, not when an image is revealed months later.
//
// Users' saved counts hang off these ids, so re-keying a coin silently moves a
// user's coins onto a different commemorative piece. Instead every scrape is
// matched back to the previous run and the id is carried forward.
package catalog

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"coinapp-backend/internal/derive"
	"coinapp-backend/internal/ecb"
)

// MatchThreshold is the similarity below which two coins in the same country and
// year are treated as different coins.
const MatchThreshold = 0.6

// fpHistory is how many past fingerprints to remember per coin.
const fpHistory = 8

var placeholderPattern = regexp.MustCompile(`(?i)placeholder`)

// Doc is one coin as stored in Firebase.
type Doc struct {
	ID       string `json:"id"`
	Country  string `json:"country"`
	Slug     string `json:"countrySlug"`
	Feature  string `json:"feature"`
	Descr    string `json:"description"`
	Volume   string `json:"volume"`
	SortVol  int64  `json:"sortVolume"`
	Date     string `json:"date"`
	Year     int    `json:"year"`
	SortDate int    `json:"sortDate"`
	// ImageURL prefers the smaller WebP. It and ImageFallback are nil for coins the
	// ECB has not revealed yet: their WebP 404s, so the client renders its own
	// placeholder instead of firing a doomed request.
	ImageURL      *string  `json:"imageUrl"`
	ImageFallback *string  `json:"imageFallbackUrl"`
	ImageMissing  bool     `json:"imagePlaceholder"`
	FP            string   `json:"fp"`
	Fingerprints  []string `json:"fps"`
	Removed       bool     `json:"removed"`
	CreatedAtMS   int64    `json:"createdAt"`
	UpdatedAtMS   int64    `json:"updatedAt"`
	// Kind is "commemorative" or "national". Documents written before this field
	// existed have it empty, which is read as commemorative; contentFields
	// includes it so the next sync backfills them.
	Kind string `json:"kind"`
}

// Series kinds, mirrored from the ecb package. A coin belongs to exactly one.
const (
	KindCommemorative = "commemorative"
	KindNational      = "national"
)

func nowMS() int64 { return time.Now().UnixMilli() }

// Change describes one altered coin.
type Change struct {
	ID      string   `json:"id"`
	Country string   `json:"country"`
	Feature string   `json:"feature"`
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Fields  []string `json:"fields,omitempty"`
	Score   float64  `json:"score,omitempty"`
}

// Report summarises what a sync did, so a run is never silent.
type Report struct {
	Added     []Change `json:"added"`
	Updated   []Change `json:"updated"`
	Rematched []Change `json:"rematched"`
	Removed   []Change `json:"removed"`
	Unchanged int      `json:"unchanged"`
}

func (r Report) Changed() bool {
	return len(r.Added) > 0 || len(r.Updated) > 0 || len(r.Rematched) > 0 || len(r.Removed) > 0
}

// String renders the report for the log.
func (r Report) String() string {
	return fmt.Sprintf(
		"added=%d updated=%d unchanged=%d rematched=%d removed=%d",
		len(r.Added), len(r.Updated), r.Unchanged, len(r.Rematched), len(r.Removed))
}

// IsPlaceholder reports whether an ECB image URL is a "coming soon" placeholder.
func IsPlaceholder(u string) bool { return placeholderPattern.MatchString(u) }

func stringPtr(s string) *string { return &s }

// ToDocument normalises a scraped coin into the stored shape, minus the id.
func ToDocument(coin ecb.Coin) Doc {
	year, _ := derive.ParseYear(coin.Date)
	sortDate, _ := derive.ParseSortDate(coin.Date)

	var webp, jpeg *string
	if len(coin.ImageURLs) > 0 {
		first := coin.ImageURLs[0]
		if !IsPlaceholder(first) {
			webp = stringPtr(first)
			if len(coin.ImageURLs) > 1 && coin.ImageURLs[1] != first {
				jpeg = stringPtr(coin.ImageURLs[1])
			}
		}
	}

	return Doc{
		Country:       coin.Country,
		Slug:          derive.Slugify(coin.Country),
		Feature:       coin.Feature,
		Descr:         coin.Description,
		Volume:        coin.Volume,
		SortVol:       derive.ParseSortVolume(coin.Volume),
		Date:          coin.Date,
		Year:          year,
		SortDate:      sortDate,
		ImageURL:      webp,
		ImageFallback: jpeg,
		ImageMissing:  webp == nil,
		FP:            derive.Fingerprint(coin.Country, year, coin.Feature),
		Kind:          kindOf(coin),
	}
}

// kindOf normalises the series, defaulting to commemorative.
//
// The default matters: every document written before the two-series split has an
// empty Kind, and treating that as "national" would hide the whole catalog.
func kindOf(coin ecb.Coin) string {
	if coin.Kind == KindNational {
		return KindNational
	}
	return KindCommemorative
}

var contentFields = []string{
	"country", "countrySlug", "feature", "description", "volume",
	"sortVolume", "date", "year", "sortDate", "imageUrl", "imageFallbackUrl", "imagePlaceholder",
	"kind",
}

// mintID returns 16 hex chars = 64 bits, collision-free across a catalog of
// this size. Readability is deliberately not a goal: stability is.
func mintID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic("catalog: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func overrideKey(d Doc) string {
	return fmt.Sprintf("%s|%d|%s", d.Slug, d.Year, derive.Normalize(d.Feature))
}

type indexes struct {
	byFingerprint map[string]string
	// Buckets are keyed by series as well as country and year, so the two series
	// can never be matched against each other. See resolve.
	byBucket     map[string][]Doc
	fingerprintK map[string]string
	overrides    map[string]string
}

// seriesBucketKey scopes a bucket to one series. The series is part of the key
// rather than a filter, so a national coin cannot pick up a commemorative id
// even when country, year and wording coincide.
func seriesBucketKey(kind, slug string, year int) string {
	return fmt.Sprintf("%s|%s|%d", kindOfSeries(kind), slug, year)
}

func kindOfSeries(kind string) string {
	if kind == KindNational {
		return KindNational
	}
	return KindCommemorative
}

func buildIndexes(existing map[string]Doc, overrides map[string]string) indexes {
	idx := indexes{
		byFingerprint: map[string]string{},
		byBucket:      map[string][]Doc{},
		fingerprintK:  map[string]string{},
		overrides:     overrides,
	}
	for id, doc := range existing {
		kind := kindOfDoc(doc)
		all := append([]string{doc.FP}, doc.Fingerprints...)
		for _, fp := range all {
			if fp == "" {
				continue
			}
			if _, seen := idx.byFingerprint[fp]; !seen {
				idx.byFingerprint[fp] = id
			}
			// Scoped copy: the same fingerprint can legitimately exist in both
			// series (same country, same year, similar wording).
			seriesFP := kind + "|" + fp
			if _, seen := idx.fingerprintK[seriesFP]; !seen {
				idx.fingerprintK[seriesFP] = id
			}
		}
		idx.byBucket[seriesBucketKey(doc.Kind, doc.Slug, doc.Year)] =
			append(idx.byBucket[seriesBucketKey(doc.Kind, doc.Slug, doc.Year)], doc)
	}
	return idx
}

// resolve picks the id for one scraped coin.
//
// Order: manual override -> exact fingerprint -> fuzzy within country+year -> new.
// `taken` prevents two scrapes in one run from claiming the same id.
//
// Every lookup is scoped to the coin's series. The two series share a country and
// often a year, so an unscoped fuzzy match would happily hand a regular 2 euro
// coin the id of a commemorative one and silently repoint a user's saved count
// at a different piece. Scoping it also means a national coin still matches the
// national coin stored by the previous run, which is what keeps its id — and
// therefore the counts attached to it — stable across syncs.
func resolve(doc Doc, idx indexes, taken map[string]struct{}) (id, how string, score float64) {
	kind := kindOfDoc(doc)

	if pinned, ok := idx.overrides[overrideKey(doc)]; ok {
		if _, used := taken[pinned]; !used {
			return pinned, "override", 1
		}
	}

	if exact, ok := idx.fingerprintK[kind+"|"+doc.FP]; ok {
		if _, used := taken[exact]; !used {
			return exact, "fingerprint", 1
		}
	}

	candidates := idx.byBucket[seriesBucketKey(doc.Kind, doc.Slug, doc.Year)]
	var (
		bestID    string
		bestScore float64
	)
	for _, candidate := range candidates {
		if _, used := taken[candidate.ID]; used {
			continue
		}
		s := derive.Similarity(candidate.Feature, doc.Feature)
		if s > bestScore {
			bestID, bestScore = candidate.ID, s
		}
	}
	if bestID != "" && bestScore >= MatchThreshold {
		return bestID, "similarity", bestScore
	}

	return "", "new", bestScore
}

func diffFields(before, after Doc) []string {
	var changed []string
	for _, field := range contentFields {
		switch field {
		case "country":
			if before.Country != after.Country {
				changed = append(changed, field)
			}
		case "countrySlug":
			if before.Slug != after.Slug {
				changed = append(changed, field)
			}
		case "feature":
			if before.Feature != after.Feature {
				changed = append(changed, field)
			}
		case "description":
			if before.Descr != after.Descr {
				changed = append(changed, field)
			}
		case "volume":
			if before.Volume != after.Volume {
				changed = append(changed, field)
			}
		case "sortVolume":
			if before.SortVol != after.SortVol {
				changed = append(changed, field)
			}
		case "date":
			if before.Date != after.Date {
				changed = append(changed, field)
			}
		case "year":
			if before.Year != after.Year {
				changed = append(changed, field)
			}
		case "sortDate":
			if before.SortDate != after.SortDate {
				changed = append(changed, field)
			}
		case "imageUrl":
			if ptrString(before.ImageURL) != ptrString(after.ImageURL) {
				changed = append(changed, field)
			}
		case "imageFallbackUrl":
			if ptrString(before.ImageFallback) != ptrString(after.ImageFallback) {
				changed = append(changed, field)
			}
		case "imagePlaceholder":
			if before.ImageMissing != after.ImageMissing {
				changed = append(changed, field)
			}
		case "kind":
			// An empty Kind means "written before the split", so it is compared as
			// commemorative rather than as a difference.
			if kindOfDoc(before) != kindOfDoc(after) {
				changed = append(changed, field)
			}
		}
	}
	return changed
}

// kindOfDoc is the stored series, defaulting to commemorative for documents
// written before the field existed.
func kindOfDoc(d Doc) string {
	if d.Kind == KindNational {
		return KindNational
	}
	return KindCommemorative
}

func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func mergeFingerprints(next string, previous Doc) []string {
	all := []string{next}
	if len(previous.Fingerprints) > 0 {
		all = append(all, previous.Fingerprints...)
	} else if previous.FP != "" {
		all = append(all, previous.FP)
	}

	seen := map[string]struct{}{}
	out := make([]string, 0, len(all))
	for _, fp := range all {
		if fp == "" {
			continue
		}
		if _, dup := seen[fp]; dup {
			continue
		}
		seen[fp] = struct{}{}
		out = append(out, fp)
		if len(out) == fpHistory {
			break
		}
	}
	return out
}

// Build matches a fresh scrape against the catalog currently in Firebase.
//
// Coins the ECB has withdrawn are tombstoned with Removed=true rather than
// deleted, so ids stay valid for anyone still holding them and a restored coin
// reappears with its original id.
func Build(scraped []ecb.Coin, existing map[string]Doc, overrides map[string]string) (map[string]Doc, Report) {
	idx := buildIndexes(existing, overrides)
	taken := map[string]struct{}{}
	now := nowMS()

	out := make(map[string]Doc, len(scraped))
	var report Report

	for _, coin := range scraped {
		doc := ToDocument(coin)

		id, how, score := resolve(doc, idx, taken)
		if id == "" {
			id = mintID()
		}
		taken[id] = struct{}{}

		previous, existed := existing[id]

		switch {
		case !existed:
			report.Added = append(report.Added, Change{ID: id, Country: doc.Country, Feature: doc.Feature})
		case how == "similarity":
			// The fingerprint moved but we are confident it is the same coin. This
			// is the case worth surfacing: it is how an ECB text edit is absorbed
			// without re-keying anything.
			report.Rematched = append(report.Rematched, Change{
				ID: id, Country: doc.Country, Feature: doc.Feature,
				From: previous.Feature, To: doc.Feature,
				Score: float64(int(score*1000)) / 1000,
			})
		}

		if existed {
			if changed := diffFields(previous, doc); len(changed) > 0 {
				report.Updated = append(report.Updated, Change{
					ID: id, Country: doc.Country, Feature: doc.Feature, Fields: changed,
				})
			} else {
				report.Unchanged++
			}
		}

		created := now
		if existed && previous.CreatedAtMS != 0 {
			created = previous.CreatedAtMS
		}

		doc.ID = id
		doc.Fingerprints = mergeFingerprints(doc.FP, previous)
		doc.Removed = false
		doc.CreatedAtMS = created
		doc.UpdatedAtMS = now
		out[id] = doc
	}

	for id, doc := range existing {
		if _, used := taken[id]; used {
			continue
		}
		// Already tombstoned by an earlier run. Carry it forward untouched and
		// stay silent about it: reporting it again would keep Report.Changed()
		// true forever, so every sync would rewrite the whole catalog to say
		// nothing had changed.
		if doc.Removed {
			out[id] = doc
			continue
		}
		doc.Removed = true
		doc.UpdatedAtMS = now
		out[id] = doc
		report.Removed = append(report.Removed, Change{ID: id, Country: doc.Country, Feature: doc.Feature})
	}

	return out, report
}

// ParseExisting reads the catalog currently in Firebase.
//
// It accepts both shapes this project has ever written:
//
//   - a positional array, which the original Java seeder produced. Every coin's
//     legacy array index becomes its id, so any counts already stored under
//     users/{uid}/owned/0, /12, /345 keep pointing at the same coin. The indexes
//     are then frozen: this package never reuses or renumbers one, so they become
//     stable opaque ids by construction.
//   - a keyed map, which this backend writes from the second run onwards.
//
// The array branch is only reachable from a pre-migration backup. cmd/rekey has
// since given every coin in the live catalog a random 16 hex character id, so
// this path now exists to read an old export rather than to read production.
func ParseExisting(raw []byte) (map[string]Doc, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]Doc{}, nil
	}

	switch trimmed[0] {
	case '[':
		var legacy []ecb.Coin
		if err := json.Unmarshal(trimmed, &legacy); err != nil {
			return nil, fmt.Errorf("decoding legacy array catalog: %w", err)
		}
		out := make(map[string]Doc, len(legacy))
		for index, entry := range legacy {
			doc := ToDocument(entry)
			id := strconv.Itoa(index)
			doc.ID = id
			doc.Fingerprints = []string{doc.FP}
			doc.Kind = KindCommemorative
			out[id] = doc
		}
		return out, nil

	case '{':
		out := map[string]Doc{}
		if err := json.Unmarshal(trimmed, &out); err != nil {
			return nil, fmt.Errorf("decoding catalog: %w", err)
		}
		return out, nil

	default:
		return nil, fmt.Errorf("unexpected catalog shape starting with %q", trimmed[0])
	}
}

// SortedIDs returns catalog keys in a stable order, for logging and tests.
func SortedIDs(catalog map[string]Doc) []string {
	ids := make([]string, 0, len(catalog))
	for id := range catalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
