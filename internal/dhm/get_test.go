package dhm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// getServer answers every request with status, content type and body.
func getServer(t *testing.T, status int, contentType, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func wantGetReason(t *testing.T, err error, reason string) {
	t.Helper()
	if FailureReason(err) != reason {
		t.Fatalf("err %v, want FetchError %q", err, reason)
	}
}

var jsonTypes = []string{"application/json"}

func TestGetBodyAcceptsOnlyAnOKBodyOfAWantedType(t *testing.T) {
	srv, _ := getServer(t, 200, "application/json;charset=UTF-8", `{"ok":true}`)
	hc := NewNoRedirectClient(time.Second)
	data, err := GetBody(context.Background(), hc, srv.URL, "application/json", jsonTypes, 64)
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("%q %v", data, err)
	}

	bad, _ := getServer(t, 500, "text/html", "<h1>secret upstream text</h1>")
	_, err = GetBody(context.Background(), hc, bad.URL, "application/json", jsonTypes, 64)
	wantGetReason(t, err, "status 500")
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("error carries upstream text")
	}

	html, _ := getServer(t, 200, "text/html", "<html>maintenance</html>")
	_, err = GetBody(context.Background(), hc, html.URL, "application/json", jsonTypes, 64)
	wantGetReason(t, err, "unexpected content type")
}

func TestGetBodyBoundsTheBody(t *testing.T) {
	srv, _ := getServer(t, 200, "application/json", "0123456789")
	hc := NewNoRedirectClient(time.Second)
	if _, err := GetBody(context.Background(), hc, srv.URL, "", jsonTypes, 10); err != nil {
		t.Fatalf("body of exactly maxBody: %v", err)
	}
	_, err := GetBody(context.Background(), hc, srv.URL, "", jsonTypes, 9)
	wantGetReason(t, err, "oversize response")
}

func TestGetBodyNeverFollowsARedirect(t *testing.T) {
	target, hits := getServer(t, 200, "application/json", "{}")
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
	}))
	defer redirect.Close()
	_, err := GetBody(context.Background(), NewNoRedirectClient(time.Second), redirect.URL, "", jsonTypes, 64)
	wantGetReason(t, err, "status 301")
	if hits.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestGetBodySendsAcceptAndNothingElse(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	if _, err := GetBody(context.Background(), NewNoRedirectClient(time.Second), srv.URL, "application/json", jsonTypes, 64); err != nil {
		t.Fatal(err)
	}
	if got.Get("Accept") != "application/json" || got.Get("Cookie") != "" || got.Get(tokenHeader) != "" {
		t.Fatalf("headers %v", got)
	}
}

func TestGetBodyCancelAndTimeoutAreCategories(t *testing.T) {
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer stall.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := GetBody(ctx, NewNoRedirectClient(5*time.Second), stall.URL, "", jsonTypes, 64)
	wantGetReason(t, err, "canceled")

	start := time.Now()
	_, err = GetBody(context.Background(), NewNoRedirectClient(50*time.Millisecond), stall.URL, "", jsonTypes, 64)
	wantGetReason(t, err, "timeout")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}
