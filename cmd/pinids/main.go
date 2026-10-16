// Command pinids writes overrides.json so a coin keeps its id across a change
// to the fields that identify it.
//
// The catalog identifies a coin by (country, year, feature), both through its
// fingerprint and through the bucket it is matched in. Giving the national 2
// euro designs their real issue years therefore made 21 of them unmatchable:
// the dry run reported added=21 removed=21, which would have silently detached
// whatever those users had counted.
//
// An override pins the id explicitly, keyed on the new field values, so the sync
// updates those documents in place instead of minting replacements.
//
// The pairing is done on the image URL rather than on the text, because the
// image is the one field that identifies a design independently of how it is
// described. Nothing is guessed: a national design with no stored image, or a
// stored image no longer on the page, is reported and left alone.
//
// Usage:
//
//	pinids              print the overrides that would be written
//	pinids --write      write overrides.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"

	"coinapp-backend/internal/catalog"
	"coinapp-backend/internal/ecb"
	"coinapp-backend/internal/firebase"
)

func main() {
	log.SetFlags(0)

	var write bool
	flag.BoolVar(&write, "write", false, "write overrides.json instead of printing it")
	flag.Parse()

	if !firebase.HasCredentials() {
		log.Fatal("no credentials: set GOOGLE_APPLICATION_CREDENTIALS and FIREBASE_DATABASE_URL")
	}
	ctx := context.Background()
	client, err := firebase.New(ctx)
	if err != nil {
		log.Fatalf("client: %v", err)
	}

	raw, err := client.ReadCatalogRaw(ctx)
	if err != nil {
		log.Fatalf("reading catalog: %v", err)
	}
	existing, err := catalog.ParseExisting(raw)
	if err != nil {
		log.Fatalf("decoding catalog: %v", err)
	}

	scraped, _, err := ecb.FetchCoins(ctx)
	if err != nil {
		log.Fatalf("scraping: %v", err)
	}

	// Index the stored national coins by their image, which is the only field
	// that survives a change to the wording or the year.
	byImage := map[string]catalog.Doc{}
	for _, doc := range existing {
		if doc.Kind == catalog.KindNational && doc.ImageURL != nil {
			byImage[*doc.ImageURL] = doc
		}
	}

	overrides := map[string]string{}
	var unresolved []string
	for _, coin := range scraped {
		if coin.Kind != ecb.KindNational || len(coin.ImageURLs) == 0 {
			continue
		}
		doc, ok := byImage[coin.ImageURLs[0]]
		if !ok {
			unresolved = append(unresolved, fmt.Sprintf("%s / %s (%s)", coin.Country, coin.Feature, coin.ImageURLs[0]))
			continue
		}
		fresh := catalog.ToDocument(coin)
		overrides[overrideKeyOf(fresh)] = doc.ID
	}

	sort.Strings(unresolved)
	for _, u := range unresolved {
		log.Printf("unresolved, left alone: %s", u)
	}

	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(overrides))
	for _, k := range keys {
		out[k] = overrides[k]
	}

	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		log.Fatalf("encoding: %v", err)
	}
	body = append(body, '\n')

	if !write {
		fmt.Printf("%d override(s)\n%s", len(out), body)
		return
	}
	if err := os.WriteFile("overrides.json", body, 0o644); err != nil {
		log.Fatalf("writing overrides.json: %v", err)
	}
	log.Printf("wrote overrides.json with %d entries", len(out))
}

// overrideKeyOf mirrors the catalog package's unexported override key, which is
// (countrySlug, year, normalised feature).
func overrideKeyOf(doc catalog.Doc) string {
	return fmt.Sprintf("%s|%d|%s", doc.Slug, doc.Year, normalizeFeature(doc.Feature))
}

func normalizeFeature(v string) string {
	out := make([]rune, 0, len(v))
	space := false
	for _, r := range []rune(v) {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f':
			space = true
		default:
			if space && len(out) > 0 {
				out = append(out, ' ')
			}
			space = false
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			out = append(out, r)
		}
	}
	return string(out)
}
