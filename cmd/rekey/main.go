// Command rekey replaces the legacy numeric coin ids with minted hex ids.
//
// The first 502 coins were seeded from a positional array, so their ids are the
// array index ("0".."501"). Every coin discovered since has a random 16 hex
// character id. This command makes the catalog uniform without changing what any
// id *means*, and without anyone having to touch their collection.
//
// The hazard is that a user's counts hang off the coin id:
// /users/{uid}/owned/<coinId> = count. Renaming a coin id without moving those
// counts silently zeroes them, and a half-finished run would leave some users
// migrated and some not.
//
// Three properties make it safe:
//
//   - Every write is a single PATCH. The Realtime Database applies a multi-path
//     update atomically, so a user's whole count map is renamed in one step or
//     not at all. There is no window where half a collection is on the old keys.
//   - The catalog is swapped first. Afterwards the database rule
//     `root.child('coins').child($coinId).exists()` rejects any write naming an
//     old id, so a client still holding a cached catalog cannot resurrect one.
//   - The old-to-new mapping is written to disk before anything is applied, so a
//     run that dies half way is resumed with the same ids rather than minting new
//     ones and orphaning the counts it already moved.
//
// Usage:
//
//	rekey                      plan only; prints what would change
//	rekey --apply              perform the migration
//	rekey --apply -map m.json  resume with a mapping from an earlier run
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"

	"coinapp-backend/internal/firebase"
)

var (
	decimalID = regexp.MustCompile(`^[0-9]+$`)
	// A minted id is exactly 16 hex characters. This has to be checked alongside
	// the decimal test, because 16 hex characters are sometimes all digits:
	// "5973505629001386" is a perfectly good minted id that a plain ^[0-9]+$
	// would misread as a legacy array index, sending it through the migration a
	// second time and detaching its count.
	hexID = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// isLegacyID reports whether an id is one of the original array indexes.
//
// Legacy ids are indexes, so they are small; a 16 character run of digits is a
// minted id that happens to look numeric.
func isLegacyID(id string) bool {
	return decimalID.MatchString(id) && !hexID.MatchString(id)
}

type options struct {
	apply     bool
	mapPath   string
	backupDir string
}

func main() {
	log.SetFlags(log.LstdFlags)

	var opts options
	flag.BoolVar(&opts.apply, "apply", false, "perform the migration; without it nothing is written")
	flag.StringVar(&opts.mapPath, "map", "id-migration.json", "where the old-to-new id mapping is kept")
	flag.StringVar(&opts.backupDir, "backup-dir", ".", "where the pre-migration backup is written")
	flag.Parse()

	if !firebase.HasCredentials() {
		log.Fatal("no credentials: set GOOGLE_APPLICATION_CREDENTIALS and FIREBASE_DATABASE_URL")
	}

	ctx := context.Background()
	client, err := firebase.New(ctx)
	if err != nil {
		log.Fatalf("client: %v", err)
	}

	mapping, coins, err := plan(ctx, client, opts.mapPath)
	if err != nil {
		log.Fatalf("plan: %v", err)
	}

	users, err := readUsers(ctx, client)
	if err != nil {
		log.Fatalf("reading users: %v", err)
	}

	report(log.Default(), mapping, coins, users, opts.apply)

	if !opts.apply {
		fmt.Println("\ndry run; nothing written. Re-run with --apply.")
		return
	}

	if err := run(ctx, client, opts, mapping, coins, users); err != nil {
		log.Fatalf("apply: %v", err)
	}
}

// plan builds the old-to-new mapping, reusing any mapping already on disk so a
// resumed run cannot mint a second set of ids.
func plan(ctx context.Context, client *firebase.Client, mapPath string) (map[string]string, map[string]json.RawMessage, error) {
	raw, err := client.ReadCatalogRaw(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reading catalog: %w", err)
	}
	coins, err := decodeCoinMap(raw)
	if err != nil {
		return nil, nil, err
	}

	mapping := map[string]string{}
	if existing, err := readMapping(mapPath); err == nil {
		mapping = existing
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("reading %s: %w", mapPath, err)
	}

	// Every numeric id must end up with a mapping, whether or not one is on disk.
	var pending []string
	for id := range coins {
		if isLegacyID(id) {
			if _, ok := mapping[id]; !ok {
				pending = append(pending, id)
			}
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		// Numeric, not lexical, so "10" sorts before "9" in the log.
		a, _ := strconvAtoi(pending[i])
		b, _ := strconvAtoi(pending[j])
		return a < b
	})
	for _, old := range pending {
		mapping[old] = mintID()
	}
	if len(pending) > 0 {
		// Written before anything is applied: this is the resume point.
		if err := writeMapping(mapPath, mapping); err != nil {
			return nil, nil, fmt.Errorf("writing %s: %w", mapPath, err)
		}
	}
	return mapping, coins, nil
}

func readMapping(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func writeMapping(path string, mapping map[string]string) error {
	body, err := json.MarshalIndent(mapping, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

func readUsers(ctx context.Context, client *firebase.Client) (map[string]map[string]int, error) {
	var raw map[string]struct {
		Owned map[string]int `json:"owned"`
	}
	if err := client.Get(ctx, "users", &raw); err != nil {
		return nil, err
	}
	out := map[string]map[string]int{}
	for uid, node := range raw {
		if len(node.Owned) > 0 {
			out[uid] = node.Owned
		}
	}
	return out, nil
}

func report(log *log.Logger, mapping map[string]string, coins map[string]json.RawMessage, users map[string]map[string]int, apply bool) {
	// Count what is still outstanding, not what the mapping file records. The
	// mapping is historical once a run finishes, and reporting its size would
	// suggest a re-run has work to do when it has none.
	pendingIDs, pendingCounts := 0, 0
	for id := range coins {
		if isLegacyID(id) {
			pendingIDs++
		}
	}
	for _, owned := range users {
		for id := range owned {
			if isLegacyID(id) {
				pendingCounts++
			}
		}
	}

	mode := "PLAN"
	if apply {
		mode = "APPLY"
	}
	log.Printf("%s: %d legacy coin ids and %d count entries across %d users still to move (mapping records %d ids)",
		mode, pendingIDs, pendingCounts, len(users), len(mapping))
	if pendingIDs == 0 && pendingCounts == 0 {
		log.Printf("  nothing outstanding; a re-run would be a no-op")
	}
	for uid, owned := range users {
		moved := 0
		for id := range owned {
			if isLegacyID(id) {
				moved++
			}
		}
		log.Printf("  %s: %d of %d entries on a legacy id", uid, moved, len(owned))
	}
}

func run(ctx context.Context, client *firebase.Client, opts options, mapping map[string]string, coins map[string]json.RawMessage, users map[string]map[string]int) error {
	backup := map[string]any{"coins": coins, "users": users, "map": mapping}
	body, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}
	backupPath := fmt.Sprintf("%s/id-migration-backup.json", opts.backupDir)
	if err := os.WriteFile(backupPath, body, 0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	log.Printf("backup written to %s", backupPath)

	// Catalog first, in one atomic patch. Each new key carries the document with
	// its own id field corrected, and each old key is nulled in the same update.
	patch := map[string]any{}
	for old, newID := range mapping {
		doc, ok := coins[old]
		if !ok {
			// Already migrated by an earlier run.
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(doc, &fields); err != nil {
			return fmt.Errorf("coin %s: %w", old, err)
		}
		fields["id"] = newID
		patch[newID] = fields
		patch[old] = nil
	}
	if len(patch) > 0 {
		if err := client.Patch(ctx, "coins", patch); err != nil {
			return fmt.Errorf("patching catalog: %w", err)
		}
		log.Printf("catalog: %d ids renamed in one atomic update", len(patch)/2)
	}

	// Then each user, one atomic patch per user.
	for uid, owned := range users {
		userPatch := map[string]any{}
		moved := 0
		for old, count := range owned {
			newID, ok := mapping[old]
			if !ok {
				continue
			}
			userPatch[newID] = count
			userPatch[old] = nil
			moved++
		}
		if moved == 0 {
			continue
		}
		if err := client.Patch(ctx, "users/"+uid+"/owned", userPatch); err != nil {
			return fmt.Errorf("patching user %s: %w", uid, err)
		}
		log.Printf("user %s: %d entries renamed in one atomic update", uid, moved)
	}

	return verify(ctx, client, users)
}

// verify re-reads everything and checks that no count was lost or orphaned.
func verify(ctx context.Context, client *firebase.Client, before map[string]map[string]int) error {
	raw, err := client.ReadCatalogRaw(ctx)
	if err != nil {
		return fmt.Errorf("verifying catalog: %w", err)
	}
	coins, err := decodeCoinMap(raw)
	if err != nil {
		return err
	}
	for id := range coins {
		if isLegacyID(id) {
			return fmt.Errorf("coin id %q is still numeric after the migration", id)
		}
	}

	after, err := readUsers(ctx, client)
	if err != nil {
		return err
	}
	for uid, owned := range before {
		got, ok := after[uid]
		if !ok {
			return fmt.Errorf("user %s disappeared", uid)
		}
		want := 0
		for _, count := range owned {
			want += count
		}
		total := 0
		for id, count := range got {
			total += count
			if _, exists := coins[id]; !exists {
				return fmt.Errorf("user %s holds a count for %q, which is not in the catalog", uid, id)
			}
		}
		if total != want {
			return fmt.Errorf("user %s held %d coins before and %d after", uid, want, total)
		}
		log.Printf("verified %s: %d entries, %d coins, all ids present in the catalog", uid, len(got), total)
	}
	return nil
}

func decodeCoinMap(raw []byte) (map[string]json.RawMessage, error) {
	trimmed := trimJSON(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return nil, errors.New("catalog is a positional array; rekeying needs the keyed map form")
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, fmt.Errorf("decoding catalog: %w", err)
	}
	return out, nil
}

func mintID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		log.Fatal("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
