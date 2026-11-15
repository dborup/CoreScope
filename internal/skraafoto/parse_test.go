package skraafoto

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/skraafoto/skraafototest"
)

func TestParseCollections(t *testing.T) {
	body := `{"collections":[{"id":"skraafotos2017"},{"id":"skraafotos2021"},{"id":"skraafotos2025"},` +
		`{"id":"skraafotos2025test"},{"id":"TESTskraafotos2023"},{"id":"skraafotos2023"},{"id":"skraafotos2019"},` +
		`{"id":"skraafotos2021"},{"id":42},{"id":"orto2025"}],"links":[]}`
	years, err := ParseCollections([]byte(body))
	if err == nil {
		t.Fatalf("a non-string id was accepted: %v", years)
	}
	body = strings.Replace(body, `{"id":42},`, "", 1)
	years, err = ParseCollections([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(years) != "[2025 2023 2021 2019]" {
		t.Fatalf("years %v: newest first, 2017 and test collections out, no duplicates", years)
	}
	var many []string
	for y := 2019; y < 2019+2*(MaxYears+5); y += 2 {
		many = append(many, fmt.Sprintf(`{"id":"skraafotos%d"}`, y))
	}
	years, err = ParseCollections([]byte(`{"collections":[` + strings.Join(many, ",") + `]}`))
	if err != nil || len(years) != MaxYears || years[0] < years[1] {
		t.Fatalf("%d years kept, cap %d: %v", len(years), MaxYears, err)
	}
	for _, bad := range []string{``, `[]`, `{"collections":{}}`, `{"collections":[]}`, `null`} {
		if _, err := ParseCollections([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseSearch(t *testing.T) {
	u := &skraafototest.Upstream{}
	body := `{"type":"FeatureCollection","features":[` + u.Feature("skraafotos2025", "west") + `]}`
	items, _, err := ParseSearch([]byte(body), 2025, West)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	it := &items[0]
	want := Item{ID: skraafototest.ItemID("skraafotos2025", "west"), Collection: "skraafotos2025", Year: 2025, Direction: West,
		Datetime: time.Date(2025, 5, 7, 10, 21, 33, 0, time.UTC), Path: skraafototest.Path(skraafototest.ItemID("skraafotos2025", "west"))}
	if *it != want {
		t.Fatalf("%+v\nwant %+v", *it, want)
	}
	if items, _, err := ParseSearch([]byte(`{"type":"FeatureCollection","features":[]}`), 2025, West); len(items) != 0 || err != nil {
		t.Fatalf("no photo: %v %v", items, err)
	}
	// Several photos, in the API's order, at most SearchLimit.
	var many []string
	for i := 0; i < SearchLimit+2; i++ {
		many = append(many, strings.Replace(u.Feature("skraafotos2025", "west"), "_00003045", fmt.Sprintf("_0000304%d", i), 1))
	}
	items, skipped, err := ParseSearch([]byte(`{"features":[`+strings.Join(many, ",")+`]}`), 2025, West)
	if err != nil || skipped != 0 || len(items) != SearchLimit || items[0].ID != "2025_83_36_4_0020_00003040" || items[SearchLimit-1].ID != "2025_83_36_4_0020_00003044" {
		t.Fatalf("%d photos, %d skipped: %v", len(items), skipped, err)
	}
	// A bad candidate after the first is left out and counted, whatever
	// is wrong with it, even a shape that does not decode; the rest keep
	// their order, and the sixth is not pulled in.
	bad := map[int]string{
		1: u.Feature("skraafotos2025", "north"),
		2: strings.Replace(many[2], `"id":"2025_`, `"id":5,"x":"`, 1),
		4: `"not a feature"`,
	}
	cands := append([]string(nil), many...)
	for i, f := range bad {
		cands[i] = f
	}
	items, skipped, err = ParseSearch([]byte(`{"features":[`+strings.Join(cands, ",")+`]}`), 2025, West)
	if err != nil || skipped != len(bad) || len(items) != 2 || items[0].ID != "2025_83_36_4_0020_00003040" || items[1].ID != "2025_83_36_4_0020_00003043" {
		t.Fatalf("bad candidates: %+v, %d skipped, %v", items, skipped, err)
	}
	// A bad first candidate still fails the search: it is the photo a
	// search before M3a showed.
	for name, f := range bad {
		cands := append([]string{f}, many[1:]...)
		if items, _, err := ParseSearch([]byte(`{"features":[`+strings.Join(cands, ",")+`]}`), 2025, West); err == nil {
			t.Errorf("first candidate %d: accepted %+v", name, items)
		}
	}
	for name, mod := range map[string]func(string) string{
		"other direction": func(s string) string { return strings.Replace(s, `"direction":"west"`, `"direction":"north"`, 1) },
		"other collection": func(s string) string {
			return strings.Replace(s, `"collection":"skraafotos2025"`, `"collection":"skraafotos2023"`, 1)
		},
		"bad id":          func(s string) string { return strings.Replace(s, `"id":"2025_`, `"id":"2025/`, 1) },
		"other year id":   func(s string) string { return strings.Replace(s, `"id":"2025_`, `"id":"2023_`, 1) },
		"bad datetime":    func(s string) string { return strings.Replace(s, `2025-05-07T10:21:33Z`, `yesterday`, 1) },
		"no datetime":     func(s string) string { return strings.Replace(s, `"datetime":"2025-05-07T10:21:33Z",`, ``, 1) },
		"not json":        func(s string) string { return s[:len(s)/2] },
		"features object": func(string) string { return `{"type":"FeatureCollection","features":{}}` },
	} {
		if items, _, err := ParseSearch([]byte(mod(body)), 2025, West); err == nil {
			t.Errorf("%s: accepted %+v", name, items)
		}
	}
	// An asset href that is not an allowed CDN path keeps the photo, without
	// a preview: the official viewer link still works.
	for _, href := range []string{"https://evil.example/x.tif", "", "https://skraafoto-cdn.dataforsyningen.dk/skraafoto_server/a/b.tif?token=x"} {
		u.Href = func(string) string { return href }
		body := `{"type":"FeatureCollection","features":[` + u.Feature("skraafotos2025", "west") + `]}`
		items, _, err := ParseSearch([]byte(body), 2025, West)
		if err != nil || len(items) != 1 || items[0].Path != "" {
			t.Errorf("href %q: %+v %v", href, items, err)
		}
	}
}

func TestParseItem(t *testing.T) {
	u := &skraafototest.Upstream{}
	id := skraafototest.ItemID("skraafotos2021", "nadir")
	it, err := ParseItem([]byte(u.Feature("skraafotos2021", "nadir")), "skraafotos2021", id)
	if err != nil || it.Path != skraafototest.Path(id) || it.Direction != Nadir {
		t.Fatal(it, err)
	}
	if _, err := ParseItem([]byte(u.Feature("skraafotos2021", "north")), "skraafotos2021", id); err == nil {
		t.Fatal("another item's body was accepted")
	}
	u.Href = func(string) string { return "https://evil.example/x.tif" }
	if _, err := ParseItem([]byte(u.Feature("skraafotos2021", "nadir")), "skraafotos2021", id); err == nil {
		t.Fatal("an item whose asset is not on the CDN was accepted for a preview")
	}
}

func FuzzParseSearch(f *testing.F) {
	u := &skraafototest.Upstream{}
	f.Add([]byte(`{"type":"FeatureCollection","features":[` + u.Feature("skraafotos2025", "north") + `]}`))
	f.Add([]byte(`{"features":[{"id":"2025_x","collection":"skraafotos2025","properties":{"direction":"north","datetime":"2025-01-01T00:00:00Z"},"assets":{"data":{"href":"https://skraafoto-cdn.dataforsyningen.dk/skraafoto_server/a/2025_x.tif"}}}]}`))
	f.Add([]byte(`{"features":[]}`))
	f.Add([]byte(`{"features":[` + strings.Replace(u.Feature("skraafotos2025", "north"), `"pers:omega":1.5`, `"pers:omega":1.5,"pers:phi":0,"pers:kappa":0,"proj:shape":[2,3],"pers:crs":25832,"pers:vertical_crs":5799,"pers:perspective_center":[1,2,3],"pers:rotation_matrix":[1,0,0,0,1,0,0,0,1],"pers:interior_orientation":{"focal_length":1,"pixel_spacing":[0.1,0.1],"principal_point_offset":[0,0],"sensor_array_dimensions":[3,2]}`, 1) + `]}`))
	// The same camera with a footprint of one point: no footprint height.
	f.Add([]byte(`{"features":[` + strings.Replace(strings.Replace(u.Feature("skraafotos2025", "north"), `"pers:omega":1.5`, `"pers:omega":0,"pers:phi":0,"pers:kappa":0,"proj:shape":[2,3],"pers:crs":25832,"pers:vertical_crs":5799,"pers:perspective_center":[574500,6224500,1000],"pers:rotation_matrix":[1,0,0,0,1,0,0,0,1],"pers:interior_orientation":{"focal_length":100,"pixel_spacing":[0.1,0.1],"principal_point_offset":[0,0],"sensor_array_dimensions":[3,2]}`, 1),
		`[[[574000,6224000],[575000,6224000],[575000,6225000],[574000,6224000]]]`, `[[[574000,6224000]]]`, 1) + `]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		items, _, err := ParseSearch(body, 2025, North)
		if err != nil {
			return
		}
		if len(items) > SearchLimit {
			t.Fatalf("%d photos", len(items))
		}
		for _, it := range items {
			if !ValidItem(it.Collection, it.ID) || it.Collection != "skraafotos2025" || it.Direction != North || it.Datetime.IsZero() {
				t.Fatalf("accepted %+v", it)
			}
			if it.Path != "" {
				if p, err := CDNPath(CDNBase+it.Path, it.ID); err != nil || p != it.Path {
					t.Fatalf("path %q not a CDN path", it.Path)
				}
			}
			if c := it.Camera; c != nil {
				// Whatever was accepted projects without panicking, and
				// stays finite where it projects.
				if col, row, ok := c.Project(c.Center[0], c.Center[1], 0); ok && (!finite(col) || !finite(row)) {
					t.Fatalf("camera %+v projects to %v,%v", c, col, row)
				}
				if c.GroundOK && !finite(c.GroundM) {
					t.Fatalf("ground %v", c.GroundM)
				}
				c.Line(Ground{E: c.Center[0], N: c.Center[1]}, Ground{E: c.Center[0] + 100, N: c.Center[1]}, func(float64) float64 { return 0 }, 4)
			}
		}
	})
}

func FuzzParseCollections(f *testing.F) {
	f.Add([]byte(`{"collections":[{"id":"skraafotos2025"},{"id":"skraafotos2017"}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		years, err := ParseCollections(body)
		if err != nil {
			return
		}
		if len(years) == 0 || len(years) > MaxYears {
			t.Fatalf("%v", years)
		}
		for i, y := range years {
			if _, ok := CollectionYear(Collection(y)); !ok || i > 0 && y >= years[i-1] {
				t.Fatalf("%v", years)
			}
		}
	})
}
