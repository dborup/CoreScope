package skraafoto

import (
	"context"
	"net/http"
	"testing"

	"github.com/meshcore-analyzer/dhm"
	"github.com/meshcore-analyzer/skraafoto/skraafototest"
)

// #803: the live smoke check sends SearchURL with the token header. It
// must be exactly what STAC.Search sends.
func TestSearchURLIsWhatSearchSends(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	s := NewSTAC(skraafototest.Token, Options{Transport: u.Transport()})
	if _, err := s.Search(context.Background(), 2025, 724700, 6175800, North); err != nil {
		t.Fatal(err)
	}
	want, err := SearchURL(2025, 724700, 6175800, North)
	if err != nil {
		t.Fatal(err)
	}
	log := u.Log()
	if len(log) != 1 {
		t.Fatalf("%d requests", len(log))
	}
	if got := "https://" + log[0].Host + log[0].Path + "?" + log[0].RawQuery; got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	if got := log[0].Header.Get(dhm.TokenHeader); got != skraafototest.Token {
		t.Fatalf("%s header %q", dhm.TokenHeader, got)
	}
}

// SearchURL refuses what Search refuses, without a request.
func TestSearchURLRefusesOutsideTheAllowList(t *testing.T) {
	if _, err := SearchURL(2017, 724700, 6175800, North); err == nil {
		t.Fatal("2017 accepted")
	}
	if _, err := SearchURL(2025, 724700, 6175800, Direction("up")); err == nil {
		t.Fatal("direction up accepted")
	}
}

// #803: the live smoke check reads CDNURL with a HeaderBytes range: the
// first read of a preview.
func TestCDNURLAndHeaderBytesAreThePreviewsFirstRead(t *testing.T) {
	u := skraafototest.New()
	defer u.Close()
	u.CDNHook = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}
	path := skraafototest.Path(skraafototest.ItemID("skraafotos2025", "north"))
	if _, err := NewCDN(Options{Transport: u.Transport()}).Preview(context.Background(), path); err == nil {
		t.Fatal("preview succeeded against a failing CDN")
	}
	want, err := CDNURL(path)
	if err != nil {
		t.Fatal(err)
	}
	log := u.Log()
	if len(log) != 1 {
		t.Fatalf("%d requests", len(log))
	}
	got := "https://" + log[0].Host + log[0].Path
	if log[0].RawQuery != "" {
		got += "?" + log[0].RawQuery
	}
	if got != want {
		t.Fatalf("read %s\nwant %s", got, want)
	}
	if got, want := log[0].Header.Get("Range"), (dhm.Range{From: 0, To: HeaderBytes - 1, MaxBody: HeaderBytes}).Header(); got != want {
		t.Fatalf("Range %q, want %q", got, want)
	}
	if _, err := CDNURL("/other/x.tif"); err == nil {
		t.Fatal("a path off the CDN allow-list accepted")
	}
}
