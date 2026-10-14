package ecb

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}

func TestParseIndexPage(t *testing.T) {
	years := ParseIndexPage(fixture(t, "ecb_index.html"))
	if len(years) != 22 {
		t.Fatalf("expected 22 year pages, got %d", len(years))
	}
	if !strings.HasSuffix(years[0], "/comm_2025.en.html") {
		t.Errorf("expected newest year first, got %s", years[0])
	}
}

func TestParseYearPage2025(t *testing.T) {
	coins := ParseYearPage(fixture(t, "y2025.html"))
	if len(coins) != 36 {
		t.Fatalf("expected 36 coins, got %d", len(coins))
	}

	for _, coin := range coins {
		if coin.Country == "" || coin.Feature == "" || coin.Date == "" || coin.Volume == "" {
			t.Fatalf("incomplete coin: %+v", coin)
		}
		if len(coin.ImageURLs) == 0 {
			t.Fatalf("no image for %s / %s", coin.Country, coin.Feature)
		}
	}
}

func TestParseStripsNbspPadding(t *testing.T) {
	var slovenia *Coin
	for i, coin := range ParseYearPage(fixture(t, "y2025.html")) {
		if coin.Country == "Slovenia" {
			slovenia = &ParseYearPage(fixture(t, "y2025.html"))[i]
		}
	}
	if slovenia == nil {
		t.Fatal("Slovenia not found")
	}

	if slovenia.Feature != "The 100th anniversary of birth of Miki Muster" {
		t.Errorf("feature = %q", slovenia.Feature)
	}
	if slovenia.Date != "November 2025" {
		t.Errorf("date = %q", slovenia.Date)
	}
	if slovenia.Volume != "1 000 000 coins" {
		t.Errorf("volume = %q", slovenia.Volume)
	}
	if !strings.HasPrefix(slovenia.Description, "Three heroes of Miki Muster") {
		t.Errorf("description = %q", slovenia.Description)
	}
}

func TestParseColonOutsideStrongTag(t *testing.T) {
	// The ECB writes "<strong>Feature</strong>: 35 years of German unity" for this
	// coin. A parser that splits the raw HTML on the label misses it entirely.
	var found bool
	for _, coin := range ParseYearPage(fixture(t, "y2025.html")) {
		if coin.Feature == "35 years of German unity" {
			found = true
		}
	}
	if !found {
		t.Fatal("coin with the colon outside <strong> was dropped")
	}
}

func TestParseProseQuarterDate(t *testing.T) {
	var found bool
	for _, coin := range ParseYearPage(fixture(t, "y2025.html")) {
		if coin.Feature == "Lithuanian Ethnographic Regions – Lithuanian Minor" {
			found = true
			if coin.Date != "Fourth quarter of 2025" {
				t.Errorf("date = %q", coin.Date)
			}
		}
	}
	if !found {
		t.Fatal("Lithuanian Minor coin not found")
	}
}

func TestImageURLsHaveNoRawSpaces(t *testing.T) {
	for _, name := range []string{"y2025.html", "y2004.html"} {
		for _, coin := range ParseYearPage(fixture(t, name)) {
			for _, u := range coin.ImageURLs {
				if strings.Contains(u, " ") {
					t.Errorf("%s: raw space in %q", coin.Feature, u)
				}
				if !strings.HasPrefix(u, origin+"/euro/coins/comm/html/") {
					t.Errorf("%s: not an absolute ECB URL: %q", coin.Feature, u)
				}
			}
		}
	}
}

func TestImagePrefersWebP(t *testing.T) {
	count := 0
	for _, coin := range ParseYearPage(fixture(t, "y2025.html")) {
		if len(coin.ImageURLs) > 1 && strings.HasSuffix(coin.ImageURLs[0], ".webp") {
			count++
		}
	}
	if count == 0 {
		t.Fatal("expected WebP listed first")
	}
}

func TestParseYearPage2004(t *testing.T) {
	coins := ParseYearPage(fixture(t, "y2004.html"))
	if len(coins) != 6 {
		t.Fatalf("expected 6 coins, got %d", len(coins))
	}
	var greece bool
	for _, coin := range coins {
		if coin.Country == "Greece" && coin.Date == "March 2004" {
			greece = true
		}
	}
	if !greece {
		t.Error("Greece March 2004 not found")
	}
}

func TestDecodeEntities(t *testing.T) {
	if got := decodeEntities("Sloven&#269;ak&nbsp;A&ndash;B"); got != "Slovenčak A–B" {
		t.Errorf("got %q", got)
	}
}

func TestNoPageURLsAsImages(t *testing.T) {
	for _, name := range []string{"y2025.html", "y2004.html"} {
		for _, coin := range ParseYearPage(fixture(t, name)) {
			for _, u := range coin.ImageURLs {
				if strings.HasSuffix(strings.ToLower(u), ".html") {
					t.Errorf("%s produced a page url: %q", coin.Feature, u)
				}
			}
		}
	}
}
