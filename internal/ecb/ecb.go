// Package ecb scrapes euro commemorative coins from the European Central Bank.
//
// The parser is deliberately regex-driven rather than pulling in an HTML parser.
// The page shape we care about is narrow, and staying dependency-free keeps the
// binary small. The regexes were validated against all 22 live year pages, which
// produce 503 coins.
package ecb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	origin    = "https://www.ecb.europa.eu"
	indexURL  = origin + "/euro/coins/comm/html/index.en.html"
	assetBase = origin + "/euro/coins/comm/html/"
	// The regular (national-side) 2 euro coins live on a separate page with a
	// different shape: one box per country holding a prose description and one
	// image per design, rather than one box per coin with labelled fields.
	nationalURL = origin + "/euro/coins/2euro/html/index.en.html"
	userAgent   = "coinapp-backend (+https://github.com/coinapp)"
)

var (
	boxOpen    = regexp.MustCompile(`<div class="box"[^>]*>`)
	divToken   = regexp.MustCompile(`<div\b|</div>`)
	contentBox = regexp.MustCompile(`(?s)<div class="content-box"[^>]*>\s*<h3[^>]*>.*?</h3>\s*<div[^>]*>(.*?)</div>`)
	heading    = regexp.MustCompile(`(?s)<h3[^>]*>(.*?)</h3>`)
	// Field labels. Note the ECB puts the colon outside the <strong> for some
	// coins ("<strong>Feature</strong>:") and inside it for others
	// ("<strong>Description: </strong>"), so tags are stripped before splitting.
	fields = regexp.MustCompile(`(?:Feature|Description|Issuing volume|Issuing date)\s*:`)

	picture    = regexp.MustCompile(`(?s)<picture\b[^>]*>(.*?)</picture>`)
	imgSrc     = regexp.MustCompile(`<img\b[^>]*\bsrc="([^"]+)"`)
	sourceSet  = regexp.MustCompile(`<source\b[^>]*\bsrcset="([^"]+)"`)
	imageExt   = regexp.MustCompile(`(?i)\.(jpe?g|png|webp)$`)
	yearLink   = regexp.MustCompile(`<a href="([^"]+)" class="box"`)
	tagPattern = regexp.MustCompile(`<[^>]+>`)
	spaceRun   = regexp.MustCompile(`[[:space:]]+`)
)

var namedEntities = map[string]string{
	"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'", "nbsp": " ",
	"ndash": "–", "mdash": "—", "lsquo": "‘", "rsquo": "’",
	"ldquo": "“", "rdquo": "”", "hellip": "…", "deg": "°",
	"eacute": "é", "egrave": "è", "agrave": "à", "ccedil": "ç",
	"uuml": "ü", "ouml": "ö", "auml": "ä", "szlig": "ß", "ntilde": "ñ",
}

var entityPattern = regexp.MustCompile(`&(#x?[0-9a-fA-F]+|[a-zA-Z]+);`)

// Coin is one scraped entry, before ids and derived fields are applied.
type Coin struct {
	Country     string   `json:"country"`
	Feature     string   `json:"feature"`
	Description string   `json:"description"`
	Volume      string   `json:"volume"`
	Date        string   `json:"date"`
	ImageURLs   []string `json:"imageUrls"`
	SourcePage  string   `json:"-"`
	// Kind is "commemorative" or "national". The national coins are a different
	// series with different matching rules, so it is decided at parse time.
	Kind string `json:"kind"`
}

func decodeEntities(text string) string {
	return entityPattern.ReplaceAllStringFunc(text, func(match string) string {
		body := match[1 : len(match)-1]

		if strings.HasPrefix(body, "#") {
			var (
				code int64
				err  error
			)
			if len(body) > 2 && (body[1] == 'x' || body[1] == 'X') {
				code, err = strconv.ParseInt(body[2:], 16, 32)
			} else {
				code, err = strconv.ParseInt(body[1:], 10, 32)
			}
			if err == nil && code > 0 && code < 0x110000 {
				return string(rune(code))
			}
			return match
		}

		if replacement, ok := namedEntities[strings.ToLower(body)]; ok {
			return replacement
		}
		return match
	})
}

// toText strips tags, decodes entities and collapses whitespace.
func toText(html string) string {
	return strings.TrimSpace(spaceRun.ReplaceAllString(
		decodeEntities(tagPattern.ReplaceAllString(html, " ")), " "))
}

// extractBoxes returns the inner HTML of every <div class="box">, honouring
// nesting. A hand-rolled scan is required because RE2 has no recursive patterns.
func extractBoxes(html string) []string {
	var boxes []string
	cursor := 0

	for {
		loc := boxOpen.FindStringIndex(html[cursor:])
		if loc == nil {
			return boxes
		}

		start := cursor + loc[1]
		depth, end := 1, start

		for depth > 0 && end < len(html) {
			token := divToken.FindStringIndex(html[end:])
			if token == nil {
				break
			}
			if strings.HasPrefix(html[end+token[0]:], "</div") {
				depth--
			} else {
				depth++
			}
			end += token[1]
		}

		boxes = append(boxes, html[start:end])
		cursor = end
	}
}

func extractCountry(box string) string {
	m := heading.FindStringSubmatch(box)
	if m == nil {
		return ""
	}
	return toText(m[1])
}

func extractFields(box string) (feature, description, volume, date string, ok bool) {
	m := contentBox.FindStringSubmatch(box)
	if m == nil {
		return "", "", "", "", false
	}

	// Strip to text BEFORE splitting on the labels.
	parts := fields.Split(toText(m[1]), -1)
	if len(parts) != 5 {
		return "", "", "", "", false
	}

	return strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2]),
		strings.TrimSpace(parts[3]), strings.TrimSpace(parts[4]), true
}

// extractImages returns absolute image URLs, best format first.
//
// The ECB ships a WebP alongside the JPEG for real renders but not for
// "coming soon" placeholders, so document order puts WebP first and the client
// uses the JPEG as an onerror fallback. 17 of the current URLs contain raw
// spaces; url.Parse plus String() percent-encodes them. Extensions are
// case-insensitive: the ECB also emits ".JPG".
func extractImages(box string) []string {
	var raws []string

	for _, p := range picture.FindAllStringSubmatch(box, -1) {
		for _, m := range sourceSet.FindAllStringSubmatch(p[1], -1) {
			raws = append(raws, m[1])
		}
		for _, m := range imgSrc.FindAllStringSubmatch(p[1], -1) {
			raws = append(raws, m[1])
		}
	}
	// Older page revisions omit <picture> and emit a bare <img>.
	if len(raws) == 0 {
		for _, m := range imgSrc.FindAllStringSubmatch(box, -1) {
			raws = append(raws, m[1])
		}
	}

	var out []string
	seen := map[string]struct{}{}
	for _, raw := range raws {
		ref, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		absolute := assetBase + ref.String()
		if !imageExt.MatchString(absolute) {
			continue
		}
		if _, dup := seen[absolute]; dup {
			continue
		}
		seen[absolute] = struct{}{}
		out = append(out, absolute)
	}
	return out
}

// ParseYearPage parses one year page.
func ParseYearPage(html string) []Coin {
	var coins []Coin

	for _, box := range extractBoxes(html) {
		country := extractCountry(box)
		feature, description, volume, date, ok := extractFields(box)
		if country == "" || !ok || feature == "" || date == "" {
			continue
		}
		coins = append(coins, Coin{
			Country:     country,
			Feature:     feature,
			Description: description,
			Volume:      volume,
			Date:        date,
			ImageURLs:   extractImages(box),
			Kind:        KindCommemorative,
		})
	}
	return coins
}

// Series kinds. A coin is one or the other, never both.
const (
	KindCommemorative = "commemorative"
	KindNational      = "national"
)

var (
	// The national page's description is a run of <p> blocks directly after the
	// <h3>, with no wrapper div as the year pages have, and a trailing country
	// link after the last one. Taking the paragraphs one at a time and stopping at
	// the first non-paragraph avoids picking that link up.
	paragraph    = regexp.MustCompile(`(?is)^\s*<p[^>]*>(.*?)</p>`)
	nationalYear = regexp.MustCompile(`^(19|20)[0-9]{2}$`)
	// Files are named like Belgium_2euro_2014ph.jpg or France_2euro_2022.jpg. The
	// trailing year is when that design entered circulation, which is the closest
	// thing the page offers to an issuing date.
	nationalYearInName = regexp.MustCompile(`([12][0-9]{3})`)
)

// ParseNationalPage parses the regular 2 euro coins page into one entry per
// design.
//
// The page is structured per country, not per coin: a box holds the country's
// prose plus one image per design. Countries that changed their national design
// show several images (Belgium three, the Vatican five), and those are genuinely
// different coins a collector owns separately, so each image becomes its own
// entry with the country text as its description.
//
// The page publishes no issuing volume and no issuing date, so Volume is left
// empty and the date is the design year taken from the image filename. The UI
// renders a missing volume as a dash rather than inventing a number.
func ParseNationalPage(html string) []Coin {
	var coins []Coin

	for _, box := range extractBoxes(html) {
		country := extractCountry(box)
		if country == "" {
			continue
		}
		description := extractNationalDescription(box)
		if description == "" {
			continue
		}

		for i, src := range extractNationalImages(box) {
			year := nationalYearFor(country, src)
			coins = append(coins, Coin{
				Country:     country,
				Feature:     nationalFeature(i, year),
				Description: description,
				Volume:      "",
				Date:        year,
				ImageURLs:   []string{src},
				Kind:        KindNational,
			})
		}
	}
	return coins
}

// extractNationalDescription returns the prose that follows the country's <h3>.
//
// The paragraphs are consecutive, so they are consumed one at a time until
// something that is not a paragraph turns up. That trailing country link is not
// part of the description and would otherwise end every country's text with its
// own name.
func extractNationalDescription(box string) string {
	h := heading.FindStringIndex(box)
	if h == nil {
		return ""
	}

	rest := box[h[1]:]
	var parts []string
	for {
		m := paragraph.FindStringSubmatch(rest)
		if m == nil {
			break
		}
		parts = append(parts, toText(m[1]))
		rest = rest[len(m[0]):]
	}
	return strings.Join(parts, " ")
}

// nationalFeature names the design, e.g. "National side, 2008 design" or
// "National side" when the year is unknown. Every design now resolves a year, so
// the first form is the normal one.
func nationalFeature(index int, year string) string {
	switch {
	case year != "":
		return "National side, " + year + " design"
	case index > 0:
		return fmt.Sprintf("National side, series %d", index+1)
	default:
		return "National side"
	}
}

// designYear pulls a year out of an image filename.
func designYear(src string) string {
	name := imageName(src)
	if m := nationalYearInName.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// imageName is the last path segment of a URL, without its extension.
func imageName(src string) string {
	name := lastPathSegment(src)
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	return name
}

// lastPathSegment is the last path segment of a URL, extension included.
func lastPathSegment(src string) string {
	if i := strings.LastIndexByte(src, '/'); i >= 0 {
		return src[i+1:]
	}
	return src
}

// extractNationalImages returns the absolute image URLs of one country box, in
// document order.
//
// These sit on a bare <img> inside a <picture class="coin-cropper"> and, unlike
// the year pages whose sources are relative to their page, they are already
// root-absolute ("/euro/coins/common/shared/img/be/Belgium_2euroAl.jpg"). So
// they only need the origin; prefixing a directory as well would double the
// path and every image would 404.
func extractNationalImages(box string) []string {
	var out []string
	seen := map[string]struct{}{}

	for _, p := range picture.FindAllStringSubmatch(box, -1) {
		for _, m := range imgSrc.FindAllStringSubmatch(p[1], -1) {
			ref, err := url.Parse(strings.TrimSpace(m[1]))
			if err != nil {
				continue
			}
			absolute := origin + ref.String()
			if !imageExt.MatchString(absolute) {
				continue
			}
			if _, dup := seen[absolute]; dup {
				continue
			}
			seen[absolute] = struct{}{}
			out = append(out, absolute)
		}
	}
	return out
}

// ParseIndexPage returns the year page URLs, newest first.
func ParseIndexPage(html string) []string {
	var out []string
	for _, m := range yearLink.FindAllStringSubmatch(html, -1) {
		out = append(out, origin+m[1])
	}
	return out
}

type fetcher func(ctx context.Context, url string) (string, error)

func defaultFetch(ctx context.Context, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s -> %d", target, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// FetchCoins scrapes every year page plus the regular 2 euro coins page, and
// returns all coins plus the year page count.
func FetchCoins(ctx context.Context) ([]Coin, int, error) {
	return fetchCoinsWith(ctx, defaultFetch)
}

func fetchCoinsWith(ctx context.Context, fetch fetcher) ([]Coin, int, error) {
	index, err := fetch(ctx, indexURL)
	if err != nil {
		return nil, 0, err
	}

	years := ParseIndexPage(index)
	if len(years) == 0 {
		return nil, 0, fmt.Errorf("no year pages found on %s", indexURL)
	}

	var coins []Coin
	for _, page := range years {
		html, err := fetch(ctx, page)
		if err != nil {
			return nil, 0, fmt.Errorf("year page %s: %w", page, err)
		}
		for _, coin := range ParseYearPage(html) {
			coin.SourcePage = page
			coins = append(coins, coin)
		}
	}

	// The national coins are a separate series on a separate page. A failure
	// there must not lose the commemoratives just scraped, so it is reported but
	// the commemoratives are still returned.
	national, err := fetch(ctx, nationalURL)
	if err != nil {
		return coins, len(years), fmt.Errorf("national 2 euro page: %w", err)
	}
	for _, coin := range ParseNationalPage(national) {
		coin.SourcePage = nationalURL
		coins = append(coins, coin)
	}

	return coins, len(years), nil
}
