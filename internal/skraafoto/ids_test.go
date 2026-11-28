package skraafoto

import (
	"net/url"
	"strings"
	"testing"
)

func TestCollectionYear(t *testing.T) {
	for c, want := range map[string]int{
		"skraafotos2025": 2025, "skraafotos2019": 2019,
		// 2017 is left out (#737 Q4); everything else is not a collection.
		"skraafotos2017": 0, "skraafotos2015": 0, "skraafotos2025test": 0, "SKRAAFOTOS2025": 0,
		"skraafotos25": 0, "skraafotos20250": 0, " skraafotos2025": 0, "": 0, "skraafotos2O25": 0,
	} {
		y, ok := CollectionYear(c)
		if want == 0 && ok || want != 0 && (!ok || y != want) {
			t.Errorf("%q: %d %v, want %d", c, y, ok, want)
		}
	}
	if Collection(2023) != "skraafotos2023" {
		t.Fatal(Collection(2023))
	}
}

func TestValidItem(t *testing.T) {
	for _, c := range []struct {
		col, id string
		ok      bool
	}{
		{"skraafotos2021", "2021_83_36_1_0020_00003045", true},
		{"skraafotos2025", "2025_83_36_5_0020_00003045", true},
		{"skraafotos2023", "2021_83_36_1_0020_00003045", false}, // year prefix mismatch
		{"skraafotos2017", "2017_83_36_1_0020_00003045", false},
		{"skraafotos2021", "2021_../etc", false},
		{"skraafotos2021", "2021_83%2F36", false},
		{"skraafotos2021", "2021_", false},
		{"skraafotos2021", "2021_" + strings.Repeat("a", 65), false},
		{"skraafotos2021", "2021-83-36", false},
		{"skraafotos2021", "2021_83_36\n", false},
	} {
		if got := ValidItem(c.col, c.id); got != c.ok {
			t.Errorf("%s/%q: %v", c.col, c.id, got)
		}
	}
}

const testID = "2025_83_36_1_0020_00003045"

func TestCDNPath(t *testing.T) {
	good := "https://skraafoto-cdn.dataforsyningen.dk/skraafoto_server/COG_oblique_2025/10km_614_57/1km_6145_573/" + testID + ".tif"
	p, err := CDNPath(good, testID)
	if err != nil || p != "/skraafoto_server/COG_oblique_2025/10km_614_57/1km_6145_573/"+testID+".tif" {
		t.Fatalf("%q %v", p, err)
	}
	for _, bad := range []string{
		strings.Replace(good, "https:", "http:", 1),
		strings.Replace(good, "skraafoto-cdn.dataforsyningen.dk", "skraafoto-cdn.dataforsyningen.dk.evil.example", 1),
		strings.Replace(good, "skraafoto-cdn.dataforsyningen.dk", "skraafoto-CDN.dataforsyningen.dk", 1),
		strings.Replace(good, "skraafoto-cdn.dataforsyningen.dk", "skraafoto-cdn.dataforsyningen.dk:8443", 1),
		strings.Replace(good, "skraafoto-cdn.dataforsyningen.dk", "user:pw@skraafoto-cdn.dataforsyningen.dk", 1),
		strings.Replace(good, "skraafoto-cdn.dataforsyningen.dk", "api.dataforsyningen.dk", 1),
		good + "?token=leak",
		good + "?",
		good + "#x",
		strings.Replace(good, "COG_oblique_2025", "..", 1),
		strings.Replace(good, "COG_oblique_2025", "%2e%2e", 1),
		strings.Replace(good, "COG_oblique_2025/", "COG_oblique_2025//", 1),
		strings.Replace(good, "/skraafoto_server/", "/other/", 1),
		strings.Replace(good, testID+".tif", "2025_83_36_2_0020_00003045.tif", 1), // another photo
		strings.Replace(good, ".tif", ".tiff", 1),
		"https://skraafoto-cdn.dataforsyningen.dk/skraafoto_server/" + testID + ".tif", // no directory
		"https://skraafoto-cdn.dataforsyningen.dk/skraafoto_server/a/b/c/d/e/" + testID + ".tif",
		"//skraafoto-cdn.dataforsyningen.dk/skraafoto_server/a/" + testID + ".tif",
		"",
	} {
		if p, err := CDNPath(bad, testID); err == nil {
			t.Errorf("%q accepted as %q", bad, p)
		}
	}
}

func FuzzCDNPath(f *testing.F) {
	f.Add("https://skraafoto-cdn.dataforsyningen.dk/skraafoto_server/COG_oblique_2025/10km_614_57/1km_6145_573/"+testID+".tif", testID)
	f.Add("https://skraafoto-cdn.dataforsyningen.dk/skraafoto_server/a/"+testID+".tif?x", testID)
	f.Add("https://user@skraafoto-cdn.dataforsyningen.dk/skraafoto_server/a/b.tif", "b")
	f.Fuzz(func(t *testing.T, href, id string) {
		p, err := CDNPath(href, id)
		if err != nil {
			return
		}
		u, err := url.Parse(CDNBase + p)
		if err != nil || u.Host != CDNHost || u.Path != p || u.RawQuery != "" || !cdnPathRe.MatchString(p) ||
			!strings.HasSuffix(p, "/"+id+".tif") || strings.Contains(p, "..") {
			t.Fatalf("%q, %q accepted as %q", href, id, p)
		}
	})
}

func TestViewerLink(t *testing.T) {
	if got := ViewerLink(testID); got != "https://skraafoto.dataforsyningen.dk/?item="+testID {
		t.Fatal(got)
	}
}

// TestCDNPathLiveHrefShape pins the asset href exactly as the live STAC API
// returned it on 2026-11-15, for every collection from 2019 to 2025: the
// COGs live on skraafoto-cdn.dataforsyningen.dk, under a resolution/year/
// 10 km/1 km path. The first release assumed cdn.dataforsyningen.dk, which
// answers 404, so every preview failed with "asset not on the CDN" (#737).
func TestCDNPathLiveHrefShape(t *testing.T) {
	const id = "2025_82_24_2_0138_00000126_100mm"
	const path = "/skraafoto_server/100mm/2025/10km_622_57/1km_6223_575/" + id + ".tif"
	p, err := CDNPath("https://skraafoto-cdn.dataforsyningen.dk"+path, id)
	if err != nil || p != path {
		t.Fatalf("live href refused: %q %v", p, err)
	}
	if _, err := CDNPath("https://cdn.dataforsyningen.dk"+path, id); err == nil {
		t.Fatal("the old host must not be accepted: it is not where the photos are")
	}
}
