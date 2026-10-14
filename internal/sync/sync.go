// Package sync orchestrates one catalog refresh: scrape the ECB, match the result
// against what Firebase already holds, and write back only what actually changed.
package sync

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"coinapp-backend/internal/catalog"
	"coinapp-backend/internal/ecb"
	"coinapp-backend/internal/firebase"
)

type Overrides map[string]string

// LoadOverrides reads overrides.json if present. It is the escape hatch for the
// one case automated matching cannot decide: an ECB reword so large that the coin
// looks like a different one, where you want to pin the id by hand.
//
// Keys beginning with "//" or "_" are treated as comments, so the file can
// document itself without those entries being applied.
func LoadOverrides(path string) Overrides {
	overrides := Overrides{}

	body, err := os.ReadFile(path)
	if err != nil {
		return overrides
	}

	var raw map[string]string
	if err := json.Unmarshal(body, &raw); err != nil {
		log.Printf("WARNING: ignoring malformed overrides.json: %v", err)
		return overrides
	}

	for key, id := range raw {
		if strings.HasPrefix(key, "//") || strings.HasPrefix(key, "_") {
			continue
		}
		overrides[key] = id
	}
	return overrides
}

// Run performs one sync. It is idempotent: re-running it against an unchanged ECB
// performs no write at all and leaves every coin id untouched.
func Run(ctx context.Context, client *firebase.Client, overrides Overrides, dryRun bool) (catalog.Report, error) {
	var report catalog.Report

	log.Println("fetching ECB…")
	scraped, pages, err := ecb.FetchCoins(ctx)
	if err != nil {
		return report, err
	}
	log.Printf("parsed %d coins from %d year pages", len(scraped), pages)

	raw, err := client.ReadCatalogRaw(ctx)
	if err != nil {
		return report, err
	}
	existing, err := catalog.ParseExisting(raw)
	if err != nil {
		return report, err
	}

	if meta, err := client.ReadMeta(ctx); err == nil && meta.SyncedAt > 0 {
		log.Printf("existing catalog: %d coins (last sync %s)",
			len(existing), time.UnixMilli(meta.SyncedAt).UTC().Format(time.RFC3339))
	} else {
		log.Printf("existing catalog: %d coins (never synced)", len(existing))
	}

	built, report := catalog.Build(scraped, existing, overrides)
	log.Println(report.String())

	logChanges(report)

	if !report.Changed() {
		log.Println("no changes; skipping write")
		return report, nil
	}
	if dryRun {
		log.Println("dry run; not writing")
		return report, nil
	}

	meta := firebase.Meta{
		Count:    0,
		Total:    len(built),
		Schema:   1,
		SyncedAt: time.Now().UnixMilli(),
	}
	for _, doc := range built {
		if !doc.Removed {
			meta.Count++
		}
	}
	meta.LastSync.Added = len(report.Added)
	meta.LastSync.Updated = len(report.Updated)
	meta.LastSync.Unchanged = report.Unchanged
	meta.LastSync.Rematched = len(report.Rematched)
	meta.LastSync.Removed = len(report.Removed)

	if err := client.WriteCatalog(ctx, built, meta); err != nil {
		return report, err
	}
	log.Printf("catalog written: %d live, %d withdrawn", meta.Count, meta.Total-meta.Count)

	return report, nil
}

func logChanges(report catalog.Report) {
	for _, change := range report.Added {
		log.Printf("  + %s  %s — %s", change.ID, change.Country, change.Feature)
	}
	for _, change := range report.Rematched {
		log.Printf("  ~ %s  matched at %.3f despite a feature rewrite", change.ID, change.Score)
		log.Printf("      %q -> %q", change.From, change.To)
	}
	for _, change := range report.Updated {
		log.Printf("  ~ %s  updated %v", change.ID, change.Fields)
	}
	for _, change := range report.Removed {
		log.Printf("  - %s  withdrawn by the ECB (tombstoned, id kept): %s", change.ID, change.Feature)
	}
}
