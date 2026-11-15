package skraafoto

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/meshcore-analyzer/dhm"
)

// Item is one photo as the server keeps it: every field validated.
type Item struct {
	ID         string
	Collection string
	Year       int
	Direction  Direction
	// Datetime is when the photo was taken, in UTC.
	Datetime time.Time
	// Path is the photo's COG on the CDN (CDNPath), or "" when its asset
	// href was not one: the photo is still listed, without a preview.
	Path string
}

// The STAC v2 shapes, only the fields read. Everything else an item
// carries (geometry, pers:*, links, providers) is ignored.
type stacCollections struct {
	Collections *[]struct {
		ID string `json:"id"`
	} `json:"collections"`
}

type stacFeatureCollection struct {
	Features *[]stacFeature `json:"features"`
}

type stacFeature struct {
	ID         string `json:"id"`
	Collection string `json:"collection"`
	Properties struct {
		Datetime  string `json:"datetime"`
		Direction string `json:"direction"`
	} `json:"properties"`
	Assets struct {
		Data struct {
			Href string `json:"href"`
		} `json:"data"`
	} `json:"assets"`
}

var errBadJSON = dhm.FetchError{Reason: "bad stac response"}

// ParseCollections returns the years offered by a /collections body,
// newest first: skraafotos<year> collections from MinYear on, each once,
// at most MaxYears. Test collections and anything else are dropped. No
// year at all is an error.
func ParseCollections(body []byte) ([]int, error) {
	var cs stacCollections
	if err := json.Unmarshal(body, &cs); err != nil || cs.Collections == nil {
		return nil, errBadJSON
	}
	seen := map[int]bool{}
	var years []int
	for _, c := range *cs.Collections {
		if y, ok := CollectionYear(c.ID); ok && !seen[y] {
			seen[y] = true
			years = append(years, y)
		}
	}
	if len(years) == 0 {
		return nil, errBadJSON
	}
	sort.Sort(sort.Reverse(sort.IntSlice(years)))
	if len(years) > MaxYears {
		years = years[:MaxYears]
	}
	return years, nil
}

// ParseSearch returns the first photo of a search of year's collection
// facing d, or nil when there is none. The photo must be what was asked
// for: its collection, its direction, an id of that year, a datetime. An
// asset that is not on the CDN leaves Path empty.
func ParseSearch(body []byte, year int, d Direction) (*Item, error) {
	var fc stacFeatureCollection
	if err := json.Unmarshal(body, &fc); err != nil || fc.Features == nil {
		return nil, errBadJSON
	}
	if len(*fc.Features) == 0 {
		return nil, nil
	}
	it, err := (*fc.Features)[0].item(Collection(year))
	if err != nil {
		return nil, err
	}
	if it.Direction != d {
		return nil, errBadJSON
	}
	return &it, nil
}

// ParseItem returns photo id of collection c from an item body. A photo
// without a CDN path is an error here: the preview needs one.
func ParseItem(body []byte, c, id string) (Item, error) {
	var f stacFeature
	if err := json.Unmarshal(body, &f); err != nil {
		return Item{}, errBadJSON
	}
	it, err := f.item(c)
	if err != nil {
		return Item{}, err
	}
	if it.ID != id {
		return Item{}, errBadJSON
	}
	if it.Path == "" {
		return Item{}, errHref
	}
	return it, nil
}

// item validates a feature of collection c.
func (f *stacFeature) item(c string) (Item, error) {
	year, ok := CollectionYear(c)
	d := Direction(f.Properties.Direction)
	if !ok || f.Collection != c || !ValidItem(c, f.ID) || !d.Valid() {
		return Item{}, errBadJSON
	}
	t, err := time.Parse(time.RFC3339, f.Properties.Datetime)
	if err != nil {
		return Item{}, errBadJSON
	}
	it := Item{ID: f.ID, Collection: c, Year: year, Direction: d, Datetime: t.UTC()}
	if p, err := CDNPath(f.Assets.Data.Href, f.ID); err == nil {
		it.Path = p
	}
	return it, nil
}
