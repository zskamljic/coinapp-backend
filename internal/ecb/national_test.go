package ecb

import (
	"strings"
	"testing"
)

// The live page carries 25 country boxes and 38 design images. Both numbers are
// asserted so an upstream redesign that silently drops coins fails here rather
// than quietly shrinking the catalog.
func TestParseNationalPage(t *testing.T) {
	coins := ParseNationalPage(fixture(t, "2euro.html"))

	countries := map[string]int{}
	images := 0
	for _, coin := range coins {
		if coin.Kind != KindNational {
			t.Errorf("expected kind %q, got %q", KindNational, coin.Kind)
		}
		if coin.Country == "" || coin.Feature == "" {
			t.Fatalf("incomplete coin: %+v", coin)
		}
		if coin.Description == "" {
			t.Errorf("no description for %s / %s", coin.Country, coin.Feature)
		}
		// The page publishes neither volume nor issuing date. The date is the
		// design year carried in the image filename, which only some designs have:
		// a country's first national design is usually just "<Country>_2euro.jpg".
		if coin.Volume != "" {
			t.Errorf("national coin should have no volume, got %q", coin.Volume)
		}
		if coin.Date != "" && !nationalYear.MatchString(coin.Date) {
			t.Errorf("date must be empty or a 4 digit design year, got %q", coin.Date)
		}
		if len(coin.ImageURLs) != 1 {
			t.Fatalf("expected exactly one image per design, got %v", coin.ImageURLs)
		}
		if !strings.HasPrefix(coin.ImageURLs[0], origin+"/euro/coins/") {
			t.Errorf("image is not an absolute ECB URL: %s", coin.ImageURLs[0])
		}
		// Guard the bug where a directory base was prefixed to an already
		// root-absolute src, producing a doubled path that 404s.
		if strings.Contains(coin.ImageURLs[0], "/common/euro/coins/") {
			t.Errorf("image path is doubled: %s", coin.ImageURLs[0])
		}
		countries[coin.Country]++
		images++
	}

	if len(countries) != 25 {
		t.Errorf("expected 25 countries, got %d", len(countries))
	}
	if images != 38 {
		t.Errorf("expected 38 designs, got %d", images)
	}
}

func TestParseNationalPageMultiSeries(t *testing.T) {
	coins := ParseNationalPage(fixture(t, "2euro.html"))

	byCountry := map[string][]Coin{}
	for _, coin := range coins {
		byCountry[coin.Country] = append(byCountry[coin.Country], coin)
	}

	// Countries that changed their national design must yield one entry per
	// design. Belgium has three images: an undated original plus the 2008 and 2014
	// series, and the dated ones must not collapse into one card.
	for _, tc := range []struct {
		country  string
		designs  int
		wantYear []string
	}{
		{"Belgium", 3, []string{"2008", "2014"}},
		{"France", 2, []string{"2022"}},
		{"Monaco", 3, []string{"2001", "2006"}},
		{"Vatican City", 5, []string{"2002", "2013", "2016", "2017"}},
	} {
		got := byCountry[tc.country]
		if len(got) != tc.designs {
			t.Fatalf("%s: expected %d designs, got %d", tc.country, tc.designs, len(got))
		}
		dates := map[string]bool{}
		for _, coin := range got {
			dates[coin.Date] = true
		}
		for _, want := range tc.wantYear {
			if !dates[want] {
				t.Errorf("%s: expected a design dated %s, got %v", tc.country, want, dates)
			}
		}
		// Every design must be a distinct entry with its own image.
		seen := map[string]bool{}
		for _, coin := range got {
			if seen[coin.ImageURLs[0]] {
				t.Errorf("%s: duplicate image %s", tc.country, coin.ImageURLs[0])
			}
			seen[coin.ImageURLs[0]] = true
		}
	}
}

func TestDesignYear(t *testing.T) {
	cases := map[string]string{
		"/euro/coins/common/shared/img/be/Belgium_2euro_2014ph.jpg":      "2014",
		"/euro/coins/common/shared/img/fr/France_2euro_2022.jpg":         "2022",
		"/euro/coins/common/shared/img/at/Austria_2Euro.jpg":             "",
		"/euro/coins/common/shared/img/vc/Vatican_2euro_SedeVacante.jpg": "",
	}
	for src, want := range cases {
		if got := designYear(src); got != want {
			t.Errorf("designYear(%s) = %q, want %q", src, got, want)
		}
	}
}

// The year pages must not be affected: they are commemoratives.
func TestParseYearPageStaysCommemorative(t *testing.T) {
	coins := ParseYearPage(fixture(t, "y2025.html"))
	if len(coins) == 0 {
		t.Fatal("no coins parsed")
	}
	for _, coin := range coins {
		if coin.Kind != KindCommemorative {
			t.Fatalf("year page coin should be %q, got %q", KindCommemorative, coin.Kind)
		}
	}
}
