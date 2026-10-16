package ecb

import "strings"

// The year a country started issuing euro coins, by the year its euro cash
// entered circulation. This is the year its first national design was issued,
// which is what a design with no date in its image filename falls back to.
//
// It is a fallback, not a source. Two designs below are newer than the country's
// first series and are dated explicitly in undatedDesignYear; see the comment
// there. Everything else in the catalog is dated by its own image filename.
var eurozoneYear = map[string]string{
	// The four non euro-area issuers of their own designs, which follow their own
	// monetary agreements rather than the eurozone.
	"Monaco":       "2001",
	"San Marino":   "2001",
	"Andorra":      "2011",
	"Vatican City": "2002",

	"Austria":     "2002",
	"Belgium":     "2002",
	"Finland":     "2002",
	"France":      "2002",
	"Germany":     "2002",
	"Ireland":     "2002",
	"Italy":       "2002",
	"Luxembourg":  "2002",
	"Netherlands": "2002",
	"Portugal":    "2002",
	"Spain":       "2002",

	"Greece":    "2001", // 12 Jan
	"Slovenia":  "2007", // 1 Jan
	"Cyprus":    "2008", // 1 Jan
	"Malta":     "2008", // 1 Jan
	"Slovakia":  "2009", // 1 Jan
	"Estonia":   "2011", // 1 Jan
	"Latvia":    "2014", // 1 Jan
	"Lithuania": "2014", // 1 Jan
	"Croatia":   "2023", // 1 Jan
	"Bulgaria":  "2026", // 1 Jan
}

// undatedDesignYear dates designs whose image filename carries no year and
// which are *not* the country's first series.
//
// Without this, both would fall back to the country's first euro year and be
// wrong by a decade or more:
//
//   - The Netherlands has Netherlands_2euro_2001.jpg (first series) and an
//     undated Netherlands_2Euro.jpg. The page's own prose dates the undated one:
//     "The second series, introduced in 2014, shows King Willem-Alexander".
//   - The Vatican's undated Vaticano_2euro_SedeVacante.jpg is a 2025 issue. The
//     page's prose stops at the 2017 series and does not mention it at all, so
//     this one date is not from the page and is kept here deliberately.
var undatedDesignYear = map[string]map[string]string{
	"Netherlands":  {"netherlands_2euro.jpg": "2014"},
	"Vatican City": {"vaticano_2euro_sedevacante.jpg": "2025"},
}

// nationalYearFor resolves the issue year of one national design.
//
// Sources, most reliable first: the image filename, then the overrides above for
// the designs that are not a country's first series, then the year the country
// began issuing euro coins. The override keys are the image filenames as the ECB
// writes them, matched case-insensitively.
func nationalYearFor(country, imageURL string) string {
	if year := designYear(imageURL); year != "" {
		return year
	}
	if overrides, ok := undatedDesignYear[country]; ok {
		if year, ok := overrides[strings.ToLower(lastPathSegment(imageURL))]; ok {
			return year
		}
	}
	return eurozoneYear[country]
}
