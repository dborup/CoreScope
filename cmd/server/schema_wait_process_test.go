package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/meshcore-analyzer/dbschema"
)

// Process contract of the start-up schema wait (PR #93 staging gate): the
// real server main() runs in a child process against a real SQLite file.

const schemaWaitHelperEnv = "CORESCOPE_SCHEMA_WAIT_HELPER"

// TestSchemaWaitServerProcess is not a test on its own: the process tests
// below re-run the test binary with schemaWaitHelperEnv set, and then it runs
// the real main() with a short wait policy.
func TestSchemaWaitServerProcess(t *testing.T) {
	if os.Getenv(schemaWaitHelperEnv) != "1" {
		t.Skip("helper process for the schema-wait process tests")
	}
	deadline, err := time.ParseDuration(os.Getenv("CORESCOPE_SCHEMA_WAIT_DEADLINE"))
	if err != nil {
		t.Fatal(err)
	}
	defaultSchemaWaitPolicy = schemaWaitPolicy{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond, Factor: 1.5, Deadline: deadline, LogEvery: 500 * time.Millisecond}
	os.Args = []string{"corescope-server",
		"-config-dir", os.Getenv("CORESCOPE_SCHEMA_WAIT_CFG"),
		"-db", os.Getenv("CORESCOPE_SCHEMA_WAIT_DB"),
		"-public", os.Getenv("CORESCOPE_SCHEMA_WAIT_PUBLIC"),
		"-port", os.Getenv("CORESCOPE_SCHEMA_WAIT_PORT")}
	main()
}

// syncBuffer collects the child's output and signals every write, so tests
// wait for log lines instead of polling.
type syncBuffer struct {
	mu      sync.Mutex
	b       bytes.Buffer
	written chan struct{}
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{written: make(chan struct{}, 1)} }

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.b.Write(p)
	select {
	case s.written <- struct{}{}:
	default:
	}
	return n, err
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type serverProc struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	port int
	done chan struct{}
	err  error
}

func startServerProc(t *testing.T, dbPath string, deadline time.Duration) *serverProc {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	pub := t.TempDir()
	os.WriteFile(filepath.Join(pub, "index.html"), []byte("<html>__BUST__</html>"), 0o644)
	p := &serverProc{out: newSyncBuffer(), port: port, done: make(chan struct{})}
	p.cmd = exec.Command(os.Args[0], "-test.run=^TestSchemaWaitServerProcess$", "-test.count=1")
	p.cmd.Env = append(os.Environ(),
		schemaWaitHelperEnv+"=1",
		"CORESCOPE_SCHEMA_WAIT_DEADLINE="+deadline.String(),
		"CORESCOPE_SCHEMA_WAIT_CFG="+t.TempDir(),
		"CORESCOPE_SCHEMA_WAIT_DB="+dbPath,
		"CORESCOPE_SCHEMA_WAIT_PUBLIC="+pub,
		fmt.Sprintf("CORESCOPE_SCHEMA_WAIT_PORT=%d", port))
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

func (p *serverProc) waitLog(t *testing.T, substr string, within time.Duration) {
	t.Helper()
	timeout := time.NewTimer(within)
	defer timeout.Stop()
	for !strings.Contains(p.out.String(), substr) {
		select {
		case <-p.out.written:
		case <-p.done:
			if !strings.Contains(p.out.String(), substr) {
				t.Fatalf("server exited (%v) before logging %q:\n%s", p.err, substr, p.out.String())
			}
		case <-timeout.C:
			t.Fatalf("no %q within %v:\n%s", substr, within, p.out.String())
		}
	}
}

// waitReady waits for the server's readiness log line, then for its HTTP
// listener to answer healthz 200 (the readiness line can precede the bind,
// so this one step polls, on a ticker, bounded).
func (p *serverProc) waitReady(t *testing.T) {
	t.Helper()
	p.waitLog(t, "readiness: ready=true", 60*time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
	for {
		if code, err := p.healthz(); err == nil && code == http.StatusOK {
			return
		}
		select {
		case <-tick.C:
		case <-p.done:
			t.Fatalf("server exited before answering healthz:\n%s", p.out.String())
		case <-timeout.C:
			t.Fatalf("healthz not 200 within 20 s of readiness:\n%s", p.out.String())
		}
	}
}

// notListening asserts the server is alive and its port refuses connections.
func (p *serverProc) notListening(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatalf("server exited while waiting:\n%s", p.out.String())
	default:
	}
	if code, err := p.healthz(); err == nil {
		t.Fatalf("healthz answered %d while the schema is missing", code)
	}
}

func (p *serverProc) healthz() (int, error) {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/healthz", p.port))
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (p *serverProc) waitExit(t *testing.T, within time.Duration) (int, time.Duration) {
	t.Helper()
	t0 := time.Now()
	select {
	case <-p.done:
	case <-time.After(within):
		t.Fatalf("server still running %v later:\n%s", within, p.out.String())
	}
	code := 0
	var exitErr *exec.ExitError
	if errors.As(p.err, &exitErr) {
		code = exitErr.ExitCode()
	}
	return code, time.Since(t0)
}

func fullSchemaDB(t *testing.T) string {
	t.Helper()
	path, rw := fixtureSchemaDB(t)
	if err := dbschema.Apply(rw, func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServerProcess_WaitsForTheSchemaThenStartsOnce(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	p := startServerProc(t, path, time.Minute)
	p.waitLog(t, "waiting for the ingestor", 20*time.Second)
	// Waiting: alive, not listening, so readiness is red, at the first
	// attempt and still at the periodic status line.
	p.notListening(t)
	p.waitLog(t, "still waiting for the schema", 10*time.Second)
	p.notListening(t)
	if err := dbschema.Apply(rw, func(string, ...interface{}) {}); err != nil { // the ingestor finishes
		t.Fatal(err)
	}
	p.waitLog(t, "schema ready after", 10*time.Second)
	p.waitReady(t)
	out := p.out.String()
	for substr, want := range map[string]int{
		"waiting for the ingestor": 1,
		"schema ready after":       1,
		"listening on":             1,
		"[db] transmissions=":      1,
		"schema still not ready":   0,
	} {
		if got := strings.Count(out, substr); got != want {
			t.Errorf("%q logged %d times, want %d", substr, got, want)
		}
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	if code, _ := p.waitExit(t, 30*time.Second); code != 0 {
		t.Fatalf("exit code %d after SIGTERM:\n%s", code, p.out.String())
	}
}

// A pre-#93 database: the route_mask column and the change log are both
// missing until the ingestor migrates; the server waits and then starts.
func TestServerProcess_PreRouteMaskDatabase(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	for _, q := range []string{`ALTER TABLE transmissions DROP COLUMN route_mask`, `DELETE FROM _migrations WHERE name = 'transmissions_route_mask_v1'`} {
		if _, err := rw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	p := startServerProc(t, path, time.Minute)
	p.waitLog(t, "waiting for the ingestor", 20*time.Second)
	if err := dbschema.Apply(rw, func(string, ...interface{}) {}); err != nil {
		t.Fatal(err)
	}
	p.waitLog(t, "schema ready after", 10*time.Second)
	p.waitReady(t)
	p.cmd.Process.Signal(syscall.SIGTERM)
	if code, _ := p.waitExit(t, 30*time.Second); code != 0 {
		t.Fatalf("exit code %d after SIGTERM:\n%s", code, p.out.String())
	}
}

func TestServerProcess_SIGTERMWhileWaitingExitsCleanly(t *testing.T) {
	path, _ := fixtureSchemaDB(t)
	p := startServerProc(t, path, time.Minute)
	p.waitLog(t, "waiting for the ingestor", 20*time.Second)
	p.cmd.Process.Signal(syscall.SIGTERM)
	code, took := p.waitExit(t, 5*time.Second)
	out := p.out.String()
	if code != 0 {
		t.Fatalf("exit code %d after SIGTERM while waiting:\n%s", code, out)
	}
	if took > 2*time.Second {
		t.Fatalf("SIGTERM while waiting took %v", took)
	}
	if strings.Contains(out, "schema still not ready") || strings.Contains(out, "schema not ready") || strings.Contains(out, "listening on") {
		t.Fatalf("cancellation logged a schema failure or started the server:\n%s", out)
	}
}

func TestServerProcess_DeadlineExitsNonZero(t *testing.T) {
	path, _ := fixtureSchemaDB(t)
	p := startServerProc(t, path, time.Second)
	code, _ := p.waitExit(t, 30*time.Second)
	out := p.out.String()
	if code == 0 {
		t.Fatalf("exit code 0 after the deadline:\n%s", out)
	}
	if strings.Count(out, "schema still not ready after") != 1 || strings.Contains(out, "listening on") {
		t.Fatalf("want exactly one deadline error and no start:\n%s", out)
	}
}

func TestServerProcess_PermanentSchemaErrorExitsAtOnce(t *testing.T) {
	path, rw := fixtureSchemaDB(t)
	if _, err := rw.Exec(dbschema.CreateRouteMaskChangesTableSQL); err != nil { // malformed: no index
		t.Fatal(err)
	}
	p := startServerProc(t, path, time.Minute)
	code, _ := p.waitExit(t, 30*time.Second)
	out := p.out.String()
	if code == 0 || strings.Contains(out, "waiting for the ingestor") || !strings.Contains(out, "index:"+dbschema.RouteMaskChangesTxIndex) {
		t.Fatalf("malformed change log: exit %d, want an immediate failure naming the index:\n%s", code, out)
	}
}

func TestServerProcess_NormalStartDoesNotWait(t *testing.T) {
	p := startServerProc(t, fullSchemaDB(t), time.Minute)
	p.waitReady(t)
	if strings.Contains(p.out.String(), "waiting for the ingestor") {
		t.Fatalf("a migrated database made the server wait:\n%s", p.out.String())
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	if code, _ := p.waitExit(t, 30*time.Second); code != 0 {
		t.Fatalf("exit code %d after SIGTERM:\n%s", code, p.out.String())
	}
}
