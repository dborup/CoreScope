package skraafoto

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/skraafoto/skraafototest"
)

func newTestSTAC(t *testing.T, u *skraafototest.Upstream) *STAC {
	t.Helper()
	s := NewSTAC(skraafototest.Token, Options{Transport: u.Transport()})
	if s == nil {
		t.Fatal("NewSTAC returned nil with a token")
	}
	return s
}

func TestSTAC_NoTokenNoClient(t *testing.T) {
	if NewSTAC("", Options{}) != nil || NewSTAC("  \t", Options{}) != nil {
		t.Fatal("a STAC client without a token")
	}
}

func TestSTAC_Years(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	years, err := newTestSTAC(t, u).Years(context.Background())
	if err != nil || len(years) != 4 || years[0] != 2025 || years[3] != 2019 {
		t.Fatal(years, err)
	}
}

// The one query shape of a search, as the official viewer sends it
// (src/modules/api.js queryItems): the collection's items endpoint, a
// contains(Point) and direction filter in EPSG:25832, limit 1. Nothing
// else, and the token only in its header.
func TestSTAC_SearchShape(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	s := newTestSTAC(t, u)
	it, err := s.Search(context.Background(), 2025, 574380, 6224120, East)
	if err != nil || it == nil || it.Direction != East || it.Collection != "skraafotos2025" {
		t.Fatal(it, err)
	}
	log := u.Log()
	if len(log) != 1 {
		t.Fatalf("%d requests", len(log))
	}
	r := log[0]
	if r.Method != "GET" || r.Host != STACHost || r.Path != "/rest/skraafoto_api/v2/collections/skraafotos2025/items" {
		t.Fatalf("%s %s%s", r.Method, r.Host, r.Path)
	}
	q, err := url.ParseQuery(r.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k, v := range q {
		if len(v) != 1 {
			t.Fatalf("%s repeated", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "crs,filter,filter-crs,filter-lang,limit" {
		t.Fatalf("keys %v", keys)
	}
	epsg := "http://www.opengis.net/def/crs/EPSG/0/25832"
	if q.Get("limit") != "1" || q.Get("filter-lang") != "cql-json" || q.Get("crs") != epsg || q.Get("filter-crs") != epsg {
		t.Fatalf("query %v", q)
	}
	wantFilter := `{"and":[{"contains":[{"property":"geometry"},{"type":"Point","coordinates":[574380,6224120]}]},{"eq":[{"property":"direction"},"east"]}]}`
	if q.Get("filter") != wantFilter {
		t.Fatalf("filter %s", q.Get("filter"))
	}
	if r.Header.Get("token") != skraafototest.Token || strings.Contains(r.RawQuery, skraafototest.Token) || strings.Contains(r.Path, skraafototest.Token) {
		t.Fatal("the token must travel in the token header and nowhere else")
	}
	if u.CDNRequests.Load() != 0 {
		t.Fatal("a search touched the CDN")
	}
}

func TestSTAC_SearchRefusesBadArguments(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	s := newTestSTAC(t, u)
	for _, c := range []struct {
		year int
		d    Direction
	}{{2017, North}, {2025, "up"}, {2025, ""}, {1999, North}} {
		if _, err := s.Search(context.Background(), c.year, 574380, 6224120, c.d); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if _, err := s.Item(context.Background(), "skraafotos2025", "../collections"); err == nil {
		t.Error("a bad item id was looked up")
	}
	if n := u.STACRequests.Load(); n != 0 {
		t.Fatalf("%d upstream requests for refused arguments", n)
	}
}

func TestSTAC_Item(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	id := skraafototest.ItemID("skraafotos2023", "south")
	it, err := newTestSTAC(t, u).Item(context.Background(), "skraafotos2023", id)
	if err != nil || it.ID != id || it.Path != skraafototest.Path(id) {
		t.Fatal(it, err)
	}
	log := u.Log()
	if len(log) != 1 || log[0].Path != "/rest/skraafoto_api/v2/collections/skraafotos2023/items/"+id || log[0].RawQuery != "" {
		t.Fatalf("%+v", log)
	}
	_, err = newTestSTAC(t, u).Item(context.Background(), "skraafotos2023", "2023_no_such_photo")
	if dhm.FailureReason(err) != "status 404" {
		t.Fatalf("missing item: %v", err)
	}
}

func TestSTAC_UpstreamFailures(t *testing.T) {
	for name, hook := range map[string]func(w http.ResponseWriter, r *http.Request) bool{
		"redirect": func(w http.ResponseWriter, r *http.Request) bool {
			http.Redirect(w, r, "https://"+CDNHost+"/skraafoto_server/a/x.tif", http.StatusFound)
			return true
		},
		"500": func(w http.ResponseWriter, r *http.Request) bool { http.Error(w, "boom", 500); return true },
		"html": func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>"))
			return true
		},
		"oversize": func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"collections":[` + strings.Repeat(`{"id":"skraafotos2025"},`, maxCollectionsBody/20) + `]}`))
			return true
		},
	} {
		u := skraafototest.New()
		u.STACHook = hook
		if _, err := newTestSTAC(t, u).Years(context.Background()); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if u.CDNRequests.Load() != 0 {
			t.Errorf("%s: a redirect was followed", name)
		}
		u.Close()
	}
}

func TestSTAC_Canceled(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newTestSTAC(t, u).Years(ctx); dhm.FailureReason(err) != "canceled" {
		t.Fatalf("%v", err)
	}
}
