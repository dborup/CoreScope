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
	it, err := ParseSearch([]byte(body), 2025, West)
	if err != nil || it == nil {
		t.Fatal(it, err)
	}
	want := Item{ID: skraafototest.ItemID("skraafotos2025", "west"), Collection: "skraafotos2025", Year: 2025, Direction: West,
		Datetime: time.Date(2025, 5, 7, 10, 21, 33, 0, time.UTC), Path: skraafototest.Path(skraafototest.ItemID("skraafotos2025", "west"))}
	if *it != want {
		t.Fatalf("%+v\nwant %+v", *it, want)
	}
	if it, err := ParseSearch([]byte(`{"type":"FeatureCollection","features":[]}`), 2025, West); it != nil || err != nil {
		t.Fatalf("no photo: %v %v", it, err)
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
		if it, err := ParseSearch([]byte(mod(body)), 2025, West); err == nil {
			t.Errorf("%s: accepted %+v", name, it)
		}
	}
	// An asset href that is not an allowed CDN path keeps the photo, without
	// a preview: the official viewer link still works.
	for _, href := range []string{"https://evil.example/x.tif", "", "https://cdn.dataforsyningen.dk/skraafoto_server/a/b.tif?token=x"} {
		u.Href = func(string) string { return href }
		body := `{"type":"FeatureCollection","features":[` + u.Feature("skraafotos2025", "west") + `]}`
		it, err := ParseSearch([]byte(body), 2025, West)
		if err != nil || it == nil || it.Path != "" {
			t.Errorf("href %q: %+v %v", href, it, err)
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
	f.Add([]byte(`{"features":[{"id":"2025_x","collection":"skraafotos2025","properties":{"direction":"north","datetime":"2025-01-01T00:00:00Z"},"assets":{"data":{"href":"https://cdn.dataforsyningen.dk/skraafoto_server/a/2025_x.tif"}}}]}`))
	f.Add([]byte(`{"features":[]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		it, err := ParseSearch(body, 2025, North)
		if err != nil || it == nil {
			return
		}
		if !ValidItem(it.Collection, it.ID) || it.Collection != "skraafotos2025" || it.Direction != North || it.Datetime.IsZero() {
			t.Fatalf("accepted %+v", it)
		}
		if it.Path != "" {
			if p, err := CDNPath(CDNBase+it.Path, it.ID); err != nil || p != it.Path {
				t.Fatalf("path %q not a CDN path", it.Path)
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
