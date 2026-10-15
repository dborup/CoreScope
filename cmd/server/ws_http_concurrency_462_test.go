package main

// Issue #462 follow-up — NOT the fix for #462 itself (see the PR body and
// the posted report for that investigation). While chasing the "My Mesh
// card" E2E timeout, reproducing CI's exact fixture/order/concurrency on a
// linux/amd64 container (Docker via Colima; CI's own ubuntu-latest runners
// are linux/amd64 too) repeatedly crashed the server with heap-corruption
// fatal errors from the Go runtime itself:
//
//   fatal error: found bad pointer in Go heap (incorrect use of unsafe or cgo?)
//   fatal error: concurrent map writes
//   fatal error: fault (SIGSEGV at a repeating-byte "poisoned" address)
//
// under heavy CONCURRENT traffic mixing WebSocket connect/disconnect churn
// (every SPA page load opens one, per public/app.js) with HTTP requests to
// /api/nodes/bulk-health and /api/analytics/subpaths-bulk. The crash site
// varied every time (subpathAccum.add's string compare, handleBulkHealth's
// JSON encode, ...), which is the signature of a data race corrupting the
// heap somewhere upstream — the write barrier that happens to flush next is
// whichever goroutine gets blamed, not necessarily the guilty one. It did
// NOT reproduce on darwin/arm64 (this repo's usual dev machines), under
// `-race` (its slowdown changes scheduling enough to miss the window most
// runs), or in isolation (needs the mixed HTTP+WS load, not either alone).
//
// This test is the reproduction, kept as a tool for whoever picks up that
// investigation — it is deliberately NOT part of the normal suite:
//   - skipped unless CORESCOPE_WS_HTTP_STRESS_462=1, so `go test ./...`
//     (and CI) never runs it and can never be destabilized by it.
//   - even when enabled, a crash here is the Go runtime terminating the
//     whole test binary (fatal errors are not recoverable panics), so a
//     human runs this deliberately, not CI unattended.
//
// To hunt for it: build and run on linux/amd64 (a Docker container is
// enough; CGO_ENABLED=1 with -race slows it down enough to often hide it,
// so prefer a plain race-less build for the first repro, then narrow with
// -race once you have a faster, more targeted reproduction):
//
//	CORESCOPE_WS_HTTP_STRESS_462=1 go test -run TestWSHTTPConcurrency_462 -v -timeout 120s .
//
// A clean run (no crash) is NOT proof the hazard is gone — see the timing
// sensitivity above.
import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWSHTTPConcurrency_462(t *testing.T) {
	if os.Getenv("CORESCOPE_WS_HTTP_STRESS_462") != "1" {
		t.Skip("opt-in reproduction for issue #462's follow-up heap-corruption finding; set CORESCOPE_WS_HTTP_STRESS_462=1 to run (see file header)")
	}

	srv, router := setupTestServer(t)
	router.HandleFunc("/ws", srv.hub.ServeWS) // not part of RegisterRoutes; wired separately in main.go
	httpSrv := httptest.NewServer(router)
	defer httpSrv.Close()
	wsURL := "ws" + httpSrv.URL[len("http"):]

	rounds := 80
	if v := os.Getenv("CORESCOPE_WS_HTTP_STRESS_462_ROUNDS"); v != "" {
		fmt.Sscanf(v, "%d", &rounds)
	}

	done := make(chan struct{})
	var wg []chan struct{}

	runHTTP := func() {
		ch := make(chan struct{})
		wg = append(wg, ch)
		go func() {
			defer close(ch)
			client := &http.Client{Timeout: 5 * time.Second}
			for r := 0; r < rounds; r++ {
				select {
				case <-done:
					return
				default:
				}
				for i := 0; i < 8; i++ {
					if resp, err := client.Get(httpSrv.URL + "/api/analytics/subpaths-bulk?groups=" + `[{"minLen":1,"maxLen":2,"limit":10},{"minLen":3,"maxLen":4,"limit":10}]`); err == nil {
						resp.Body.Close()
					}
					if resp, err := client.Get(httpSrv.URL + "/api/nodes/bulk-health?limit=50"); err == nil {
						resp.Body.Close()
					}
				}
			}
		}()
	}
	runWS := func() {
		ch := make(chan struct{})
		wg = append(wg, ch)
		go func() {
			defer close(ch)
			for r := 0; r < rounds; r++ {
				select {
				case <-done:
					return
				default:
				}
				for i := 0; i < 15; i++ {
					conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
					if err != nil {
						continue
					}
					time.AfterFunc(10*time.Millisecond, func() { conn.Close() })
				}
			}
		}()
	}

	for i := 0; i < 4; i++ {
		runHTTP()
		runWS()
	}
	for _, ch := range wg {
		<-ch
	}
	close(done)
}
