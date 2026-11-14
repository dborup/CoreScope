package mastedb

import (
	"math"
	"sort"
	"strings"

	"github.com/meshcore-analyzer/dhm"
)

// Site is every row published at one position. Mastedatabasen has no site
// id: one physical site is several rows (one per technology and band, often
// under several station names) at identical coordinates, so rows are
// grouped on exact coordinates.
//
// Only published fields appear. There is no operator, no height and no
// structure type, because the source publishes none of them (#724).
type Site struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
	// E and N are the published EPSG:25832 position.
	E float64 `json:"-"`
	N float64 `json:"-"`
	// The address of the lowest-id row that has a street, or of the
	// lowest-id row when none has one.
	Street      string   `json:"street,omitempty"`
	HouseNumber string   `json:"houseNumber,omitempty"`
	Postcode    string   `json:"postcode,omitempty"`
	City        string   `json:"city,omitempty"`
	Stations    []string `json:"stations"` // distinct, sorted
	Entries     []Entry  `json:"entries"`  // by row id
	// Interference is set when any entry is interference-relevant.
	Interference bool `json:"interference"`
	// LikelyMast is a heuristic, not a fact from the source: the site
	// carries a broadcast technology, and broadcast transmitters almost
	// always sit on tall dedicated masts. The owner chose to show all
	// sites honestly labelled (#724 option A), so this is data only.
	LikelyMast bool `json:"likelyMast"`
}

// Entry is one row of a site.
type Entry struct {
	Technology   string `json:"technology"`
	BandMHz      int    `json:"bandMHz,omitempty"` // absent when unknown
	Service      string `json:"service,omitempty"`
	Commissioned string `json:"commissioned,omitempty"`
	// Interference is InterferenceRelevant for this entry, so the browser
	// never re-implements the rule.
	Interference bool `json:"interference"`
}

// interferenceBandsMHz are the Frekvensbaand values whose downlinks
// (about 758–960 MHz) lie within roughly 50–60 MHz of 868 MHz.
var interferenceBandsMHz = map[int]bool{700: true, 800: true, 900: true}

// InterferenceRelevant reports whether an entry may desensitise an
// unfiltered 868 MHz receiver nearby: a 700, 800 or 900 MHz band, or GSM-R
// (railway GSM, 876–880 / 921–925 MHz) whatever band is published. Any
// other entry with an unknown band does not count.
func InterferenceRelevant(technology string, bandMHz int) bool {
	return interferenceBandsMHz[bandMHz] || strings.EqualFold(strings.TrimSpace(technology), "GSM-R")
}

// broadcastTechnologies are the Teknologi_navn values of broadcast
// transmitters, compared case-insensitively.
var broadcastTechnologies = map[string]bool{
	"DVB-T": true, "DVB-T2": true, "DAB": true, "DAB+": true, "FM": true, "TV": true,
}

// isBroadcast reports whether technology is a broadcast technology.
func isBroadcast(technology string) bool {
	return broadcastTechnologies[strings.ToUpper(strings.TrimSpace(technology))]
}

// Sites groups rows into sites by exact position. Sites are ordered by
// easting, then northing, so the output does not depend on row order.
// The cost is O(r log r) for r rows.
func Sites(rows []Row) []Site {
	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.E != b.E {
			return a.E < b.E
		}
		if a.N != b.N {
			return a.N < b.N
		}
		return a.ID < b.ID
	})
	sites := make([]Site, 0)
	for i := 0; i < len(sorted); {
		j := i
		for j < len(sorted) && sorted[j].E == sorted[i].E && sorted[j].N == sorted[i].N {
			j++
		}
		sites = append(sites, newSite(sorted[i:j]))
		i = j
	}
	return sites
}

// newSite builds one site from rows at the same position, sorted by id.
func newSite(rows []Row) Site {
	lat, lon := dhm.FromUTM32(rows[0].E, rows[0].N)
	s := Site{
		Lat:     round6(lat),
		Lon:     round6(lon),
		E:       rows[0].E,
		N:       rows[0].N,
		Entries: make([]Entry, 0, len(rows)),
	}
	addr := rows[0]
	for _, r := range rows {
		if r.Street != "" {
			addr = r
			break
		}
	}
	s.Street, s.HouseNumber, s.Postcode, s.City = addr.Street, addr.HouseNumber, addr.Postcode, addr.City
	seen := make(map[string]bool)
	for _, r := range rows {
		if r.Station != "" && !seen[r.Station] {
			seen[r.Station] = true
			s.Stations = append(s.Stations, r.Station)
		}
		e := Entry{
			Technology:   r.Technology,
			BandMHz:      r.BandMHz,
			Service:      r.Service,
			Commissioned: r.Commissioned,
			Interference: InterferenceRelevant(r.Technology, r.BandMHz),
		}
		s.Interference = s.Interference || e.Interference
		s.LikelyMast = s.LikelyMast || isBroadcast(r.Technology)
		s.Entries = append(s.Entries, e)
	}
	sort.Strings(s.Stations)
	if s.Stations == nil {
		s.Stations = []string{}
	}
	return s
}

// round6 rounds degrees to 1e-6 (about 0.1 m), which keeps the JSON short.
func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
