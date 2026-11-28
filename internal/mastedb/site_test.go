package mastedb

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"math/rand"
	"os"
	"reflect"
	"testing"

	"github.com/meshcore-analyzer/dhm"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden.json")

// TestSitesGolden is the whole M1 path for the small fixture: parse, group
// and the JSON shape M2 will serve.
func TestSitesGolden(t *testing.T) {
	page, err := Parse(readFixture(t, "cell_small.gml"), testCell)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(Sites(page.Rows), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const golden = "testdata/cell_small.golden.json"
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := readFixture(t, "cell_small.golden.json"); !bytes.Equal(got, want) {
		t.Fatalf("sites JSON differs from %s (run with -update after checking):\n%s", golden, got)
	}
}

func TestSitesGrouping(t *testing.T) {
	page, err := Parse(readFixture(t, "cell_small.gml"), testCell)
	if err != nil {
		t.Fatal(err)
	}
	sites := Sites(page.Rows)
	if len(sites) != 4 {
		t.Fatalf("%d sites, want 4", len(sites))
	}
	a := sites[0] // lowest easting: the min-corner site at 526000
	if a.E != 526000 || len(a.Entries) != 1 {
		t.Fatalf("first site %+v", a)
	}
	a = sites[1]
	if a.E != 526400 || a.N != 6254800 {
		t.Fatalf("second site at %v,%v", a.E, a.N)
	}
	if !reflect.DeepEqual(a.Stations, []string{"TEST-A1", "TEST-A2"}) {
		t.Errorf("stations %v, want distinct and sorted", a.Stations)
	}
	if len(a.Entries) != 3 || a.Entries[0].BandMHz != 800 || a.Entries[2].BandMHz != 3500 {
		t.Errorf("entries %+v, want three by row id", a.Entries)
	}
	if !a.Interference || a.LikelyMast {
		t.Errorf("LTE 800 site: Interference %v LikelyMast %v", a.Interference, a.LikelyMast)
	}
	// The GSM-R row has the lower id but no street; the address comes
	// from the next row.
	rail := sites[2]
	if rail.Street != "Banevej" || rail.HouseNumber != "3" {
		t.Errorf("rail site address %q %q, want Banevej 3", rail.Street, rail.HouseNumber)
	}
	if !rail.Interference || !rail.Entries[0].Interference || rail.Entries[0].BandMHz != 0 {
		t.Errorf("GSM-R without a band must count: %+v", rail)
	}
	radio := sites[3]
	if !radio.LikelyMast || radio.Interference {
		t.Errorf("FM/DVB-T site: LikelyMast %v Interference %v", radio.LikelyMast, radio.Interference)
	}
	// Lat/lon is the published position, unprojected.
	for _, s := range sites {
		e, n := dhm.ToUTM32(s.Lat, s.Lon)
		if math.Hypot(e-s.E, n-s.N) > 0.2 {
			t.Errorf("site %v,%v: lat/lon %v,%v projects back to %v,%v", s.E, s.N, s.Lat, s.Lon, e, n)
		}
	}
}

func TestSitesIndependentOfRowOrder(t *testing.T) {
	page, err := Parse(genGML(PageLimit, "500"), testCell)
	if err != nil {
		t.Fatal(err)
	}
	want := Sites(page.Rows)
	if len(want) != (PageLimit+5)/6 {
		t.Fatalf("%d sites from %d rows of six per site", len(want), PageLimit)
	}
	rows := append([]Row(nil), page.Rows...)
	rand.New(rand.NewSource(1)).Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	shuffled := append([]Row(nil), rows...)
	if got := Sites(rows); !reflect.DeepEqual(got, want) {
		t.Fatal("Sites depends on row order")
	}
	if !reflect.DeepEqual(rows, shuffled) {
		t.Fatal("Sites reordered its argument")
	}
}

func TestSitesEmpty(t *testing.T) {
	b, err := json.Marshal(Sites(nil))
	if err != nil || string(b) != "[]" {
		t.Fatalf("Sites(nil) marshals to %s, %v; want []", b, err)
	}
}

func TestInterferenceRelevant(t *testing.T) {
	cases := []struct {
		tech string
		band int
		want bool
	}{
		{"LTE", 700, true},
		{"5G-NR", 700, true},
		{"LTE", 800, true},
		{"GSM", 900, true},
		{"LTE", 900, true},
		{"GSM-R", 900, true},
		{"GSM-R", 0, true},
		{"gsm-r", 0, true},
		{" GSM-R ", 0, true},
		{"LTE", 0, false},
		{"", 0, false},
		{"LTE", 1500, false},
		{"LTE", 1800, false},
		{"LTE", 2100, false},
		{"5G-NR", 3500, false},
		{"5G-NR", 26000, false},
		{"FM", 100, false},
		{"LTE", 850, false},
		{"LTE", 868, false},
		{"GSM-RX", 0, false},
	}
	for _, tc := range cases {
		if got := InterferenceRelevant(tc.tech, tc.band); got != tc.want {
			t.Errorf("InterferenceRelevant(%q, %d) = %v, want %v", tc.tech, tc.band, got, tc.want)
		}
	}
}

func TestIsBroadcast(t *testing.T) {
	for _, tech := range []string{"DVB-T", "dvb-t", "DVB-T2", "DAB", "DAB+", "FM", " FM ", "TV"} {
		if !isBroadcast(tech) {
			t.Errorf("%q not broadcast", tech)
		}
	}
	for _, tech := range []string{"LTE", "5G-NR", "GSM", "GSM-R", "", "FMX"} {
		if isBroadcast(tech) {
			t.Errorf("%q counted as broadcast", tech)
		}
	}
}

func TestSiteAddress(t *testing.T) {
	at := func(id int64, street, house, postcode, city string) Row {
		return Row{ID: id, E: 527000, N: 6255000, Street: street, HouseNumber: house, Postcode: postcode, City: city}
	}
	cases := []struct {
		name string
		rows []Row
		want [4]string
	}{
		{"lowest id with a street wins",
			[]Row{at(3, "Tredjevej", "3", "8800", "C"), at(2, "Andenvej", "2", "8800", "B"), at(1, "", "", "8800", "A")},
			[4]string{"Andenvej", "2", "8800", "B"}},
		{"no street: lowest id row",
			[]Row{at(5, "", "", "8830", "Y"), at(4, "", "", "8800", "X")},
			[4]string{"", "", "8800", "X"}},
	}
	for _, tc := range cases {
		sites := Sites(tc.rows)
		if len(sites) != 1 {
			t.Fatalf("%s: %d sites", tc.name, len(sites))
		}
		s := sites[0]
		if got := [4]string{s.Street, s.HouseNumber, s.Postcode, s.City}; got != tc.want {
			t.Errorf("%s: address %q, want %q", tc.name, got, tc.want)
		}
	}
}
