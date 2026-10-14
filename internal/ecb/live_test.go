package ecb

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"coinapp-backend/internal/derive"
)

// TestLiveEcb is skipped by default because it hits the network. Run it with:
//
//	COINAPP_LIVE=1 go test ./internal/ecb/ -run TestLiveEcb -v
//
// It pins the numbers the live site currently produces, so a silent ECB layout
// change shows up as a failing assertion rather than as a quietly empty catalog.
func TestLiveEcb(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	if live := testingEnv("COINAPP_LIVE"); live == "" {
		t.Skip("set COINAPP_LIVE=1 to run the live ECB check")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	coins, pages, err := FetchCoins(ctx)
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}

	if pages != 22 {
		t.Errorf("expected 22 year pages, got %d", pages)
	}
	if len(coins) < 500 {
		t.Fatalf("only %d coins parsed; the parser has probably broken", len(coins))
	}
	t.Logf("parsed %d coins from %d pages", len(coins), pages)

	fingerprints := map[string]bool{}
	placeholder := 0

	for _, coin := range coins {
		if coin.Country == "" || coin.Feature == "" || coin.Date == "" || coin.Volume == "" {
			t.Errorf("incomplete coin: %+v", coin)
		}
		if len(coin.ImageURLs) == 0 {
			t.Errorf("no image for %s / %s", coin.Country, coin.Feature)
		}
		for _, u := range coin.ImageURLs {
			if strings.Contains(u, " ") {
				t.Errorf("raw space in %q", u)
			}
		}

		year, ok := derive.ParseYear(coin.Date)
		if !ok {
			t.Errorf("no year in %q", coin.Date)
		}
		if _, ok := derive.ParseSortDate(coin.Date); !ok {
			t.Errorf("no sort date for %q", coin.Date)
		}

		fp := derive.Fingerprint(coin.Country, year, coin.Feature)
		if fingerprints[fp] {
			t.Errorf("identity fingerprint collision: %s / %s", coin.Country, coin.Feature)
		}
		fingerprints[fp] = true

		if len(coin.ImageURLs) > 0 && strings.Contains(coin.ImageURLs[0], "placeholder") {
			placeholder++
		}
	}

	t.Logf("unrevealed coins (placeholder image): %d", placeholder)
}

func testingEnv(key string) string {
	return os.Getenv(key)
}
