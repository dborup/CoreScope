package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
)

// Process contract of the graceful shutdown (#577). ListenAndServe returns
// ErrServerClosed as soon as Shutdown closes the listener, before the drain,
// the hub close, the ping-score history stop and dbClose have run. main must
// wait for the shutdown goroutine, bounded by shutdownTimeout.

// heldRequest is a POST /api/decode whose body is withheld: the server has
// sent 100 Continue, so the handler is in flight, blocked reading the body,
// and Shutdown has an active connection to drain.
type heldRequest struct {
	conn net.Conn
	br   *bufio.Reader
}

const heldDecodeBody = `{"hex":""}`

func holdDecodeRequest(t *testing.T, port int) *heldRequest {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "POST /api/decode HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\nExpect: 100-continue\r\n\r\n", len(heldDecodeBody))
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusContinue {
		t.Fatalf("want 100 Continue (handler reading the body), got %d", resp.StatusCode)
	}
	conn.SetReadDeadline(time.Time{})
	return &heldRequest{conn: conn, br: br}
}

// finish sends the body and returns the final response's status.
func (h *heldRequest) finish(t *testing.T) int {
	t.Helper()
	if _, err := h.conn.Write([]byte(heldDecodeBody)); err != nil {
		t.Fatalf("send the held body: %v", err)
	}
	h.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	resp, err := http.ReadResponse(h.br, nil)
	if err != nil {
		t.Fatalf("the in-flight request was cut, not drained: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// waitListenerClosed waits until the port refuses connections: Shutdown has
// started, and ListenAndServe has returned in main (bounded ticker poll).
func (p *serverProc) waitListenerClosed(t *testing.T) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.port), time.Second)
		if err != nil {
			return
		}
		c.Close()
		select {
		case <-tick.C:
		case <-timeout.C:
			t.Fatalf("listener still open 10 s after SIGTERM:\n%s", p.out.String())
		}
	}
}

// assertLogOrder asserts that each line appears, in this order.
func assertLogOrder(t *testing.T, out string, lines ...string) {
	t.Helper()
	at := 0
	for _, l := range lines {
		i := strings.Index(out[at:], l)
		if i < 0 {
			t.Fatalf("%q missing or out of order (want %q):\n%s", l, lines, out)
		}
		at += i + len(l)
	}
}

// The server's DB close logs this prefix whatever the checkpoint's outcome.
const dbClosedLog = "[db] WAL checkpoint"

// assertDBReleased asserts that the writer (the ingestor's role) can
// checkpoint the whole WAL and truncate it: no reader lock or snapshot of
// the server's is left behind.
func assertDBReleased(t *testing.T, path string, rw *sql.DB) {
	t.Helper()
	var busy, logFrames, done int
	if err := rw.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil {
		t.Fatal(err)
	}
	if busy != 0 {
		t.Fatalf("writer checkpoint blocked after the server exited: busy=%d log=%d done=%d", busy, logFrames, done)
	}
	st, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 0 {
		t.Fatalf("-wal is %d bytes after a TRUNCATE checkpoint", st.Size())
	}
}

func migratedDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path, rw := fixtureSchemaDB(t)
	if err := dbschema.Apply(rw, func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	return path, rw
}

// SIGTERM with a request in flight: the request is drained, then the hub,
// the history worker and the DB are closed, and only then does the process
// exit 0. On master main returned at the listener close and exited while
// the request was still in flight.
func TestServerProcess_SIGTERMDrainsThenClosesDB_577(t *testing.T) {
	path, rw := migratedDB(t)
	p := startServerProc(t, path, time.Minute)
	p.waitReady(t)
	req := holdDecodeRequest(t, p.port)
	p.cmd.Process.Signal(syscall.SIGTERM)
	p.waitLog(t, "received terminated", 10*time.Second)
	p.waitListenerClosed(t)
	if code := req.finish(t); code != http.StatusBadRequest {
		t.Fatalf("drained request answered %d, want 400 (empty hex)", code)
	}
	if code, _ := p.waitExit(t, 30*time.Second); code != 0 {
		t.Fatalf("exit code %d after SIGTERM:\n%s", code, p.out.String())
	}
	out := p.out.String()
	assertLogOrder(t, out, "received terminated", "[ws] all clients disconnected", dbClosedLog, "[server] shutdown complete")
	if strings.Contains(out, "HTTP shutdown error") || strings.Contains(out, "did not complete") {
		t.Fatalf("a clean drain logged a shutdown failure:\n%s", out)
	}
	assertDBReleased(t, path, rw)
}

// A request that never finishes is cut off at the drain timeout; the rest
// of the sequence still runs, and the process exits 0.
func TestServerProcess_HangingDrainCutAtDrainTimeout_577(t *testing.T) {
	const drain = time.Second
	path, rw := migratedDB(t)
	p := startServerProc(t, path, time.Minute, "CORESCOPE_SHUTDOWN_DRAIN="+drain.String())
	p.waitReady(t)
	holdDecodeRequest(t, p.port) // never finished
	t0 := time.Now()
	p.cmd.Process.Signal(syscall.SIGTERM)
	code, _ := p.waitExit(t, 30*time.Second)
	took := time.Since(t0)
	out := p.out.String()
	if code != 0 {
		t.Fatalf("exit code %d after a drain timeout:\n%s", code, out)
	}
	if took < drain {
		t.Fatalf("exited %v after SIGTERM, before the %v drain timeout:\n%s", took, drain, out)
	}
	assertLogOrder(t, out, "received terminated", "HTTP shutdown error: context deadline exceeded",
		"[ws] all clients disconnected", dbClosedLog, "[server] shutdown complete")
	assertDBReleased(t, path, rw)
}

// A shutdown that outlasts shutdownTimeout (here the drain, set longer than
// it) does not keep the process up: main gives up at the bound, says so and
// exits non-zero, without the deferred stops that may be what hangs.
func TestServerProcess_ShutdownTimeoutBoundsTheWait_577(t *testing.T) {
	const bound = time.Second
	path, rw := migratedDB(t)
	p := startServerProc(t, path, time.Minute,
		"CORESCOPE_SHUTDOWN_DRAIN=1m", "CORESCOPE_SHUTDOWN_TIMEOUT="+bound.String())
	p.waitReady(t)
	holdDecodeRequest(t, p.port) // never finished
	t0 := time.Now()
	p.cmd.Process.Signal(syscall.SIGTERM)
	code, _ := p.waitExit(t, 20*time.Second)
	took := time.Since(t0)
	out := p.out.String()
	if code != 1 {
		t.Fatalf("exit code %d when the shutdown outlasts its bound, want 1:\n%s", code, out)
	}
	if took < bound {
		t.Fatalf("exited %v after SIGTERM, before the %v bound:\n%s", took, bound, out)
	}
	assertLogOrder(t, out, "received terminated", "[server] shutdown did not complete within "+bound.String())
	if strings.Contains(out, "shutdown complete") {
		t.Fatalf("the bounded wait logged a completed shutdown:\n%s", out)
	}
	assertDBReleased(t, path, rw)
}

// The bounds' order: the drain timeout fits inside the wait, so a hanging
// drain is cut and the rest of the sequence still runs, and the wait fits
// inside every compose stop_grace_period, so main reports a hung shutdown
// before the runtime's SIGKILL.
func TestShutdownBoundsFitTheStopGracePeriod_577(t *testing.T) {
	if shutdownDrainTimeout >= shutdownTimeout {
		t.Fatalf("shutdownDrainTimeout %v must be below shutdownTimeout %v", shutdownDrainTimeout, shutdownTimeout)
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "docker-compose*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no compose files found (%v): the guard is broken", err)
	}
	re := regexp.MustCompile(`(?m)^\s*stop_grace_period:\s*(\S+)`)
	seen := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			grace, err := time.ParseDuration(m[1])
			if err != nil {
				t.Fatalf("%s: stop_grace_period %q: %v", f, m[1], err)
			}
			seen++
			if shutdownTimeout >= grace {
				t.Errorf("%s: shutdownTimeout %v must be below stop_grace_period %v", f, shutdownTimeout, grace)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no stop_grace_period in any compose file: the guard is broken")
	}
}
