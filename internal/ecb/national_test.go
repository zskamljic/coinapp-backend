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
		// The page publishes no issuing volume. The date is the design's issue year,
		// which now always resolves: from the image filename, from the override for
		// designs that are not a country's first series, or from the year the country
		// began issuing euro coins.
		if coin.Volume != "" {
			t.Errorf("national coin should have no volume, got %q", coin.Volume)
		}
		if !nationalYear.MatchString(coin.Date) {
			t.Errorf("%s / %s: date must be a 4 digit year, got %q", coin.Country, coin.Feature, coin.Date)
		}
		if strings.Contains(coin.Feature, "series") {
			t.Errorf("%s: a design should never fall back to a series number: %q", coin.Country, coin.Feature)
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

// Every national design must end up with the year it was issued: the filename
// when it has one, an override when the design is newer than the country's first
// series, and otherwise the year the country began issuing euro coins.
func TestNationalYearForEveryDesign(t *testing.T) {
	cases := []struct {
		country, image, want, why string
	}{
		{"Belgium", "be/Belgium_2euro_2014ph.jpg", "2014", "filename"},
		{"France", "fr/France_2euro_2022.jpg", "2022", "filename"},
		{"Austria", "at/Austria_2Euro.jpg", "2002", "eurozone entry"},
		{"Greece", "gr/Greece_2euro.jpg", "2001", "eurozone entry"},
		{"Slovenia", "si/Slovenia_2Euro.jpg", "2007", "eurozone entry"},
		{"Croatia", "hr/Croatia_2euro.jpg", "2023", "eurozone entry"},
		{"Bulgaria", "bg/Bulgaria_2euro.jpg", "2026", "eurozone entry"},
		{"Estonia", "ee/Estonia_2Euro.jpg", "2011", "eurozone entry"},
		{"Andorra", "ad/ad_2euro.jpg", "2011", "eurozone entry"},
		{"Monaco", "mc/Monaco_2euro_2025.jpg", "2025", "filename"},
		// The two designs that are not a country's first series, where the
		// eurozone entry year would be wrong.
		{"Netherlands", "nl/Netherlands_2Euro.jpg", "2014", "override, not 2002"},
		{"Vatican City", "va/Vaticano_2euro_SedeVacante.jpg", "2025", "override, not 2002"},
		// A dated filename must win even for a country that has an override.
		{"Netherlands", "nl/Netherlands_2euro_2001.jpg", "2001", "filename beats override"},
	}
	for _, tc := range cases {
		if got := nationalYearFor(tc.country, "https://ecb.example/euro/coins/common/shared/img/"+tc.image); got != tc.want {
			t.Errorf("%s %s = %q, want %q (%s)", tc.country, tc.image, got, tc.want, tc.why)
		}
	}
}

// An unrecognised country must not invent a year.
func TestNationalYearForUnknownCountry(t *testing.T) {
	if got := nationalYearFor("Atlantis", "xx/Atlantis_2euro.jpg"); got != "" {
		t.Errorf("got %q, want an empty year for a country with no data", got)
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
