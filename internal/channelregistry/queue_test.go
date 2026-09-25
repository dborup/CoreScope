package channelregistry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func submit(id, name string, at int64) Command {
	return Command{RequestID: id, Op: OpSubmit, Name: name, CreatedAt: at}
}

func TestValidIDRejectsPathTricks(t *testing.T) {
	for _, bad := range []string{"", "abc", "../../etc/passwd", "0123456789ABCDEF", "0123456789abcdeg", "0123456789abcdef/.."} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
	id := NewID()
	if !ValidID(id) {
		t.Fatalf("NewID() = %q is not valid", id)
	}
	if NewID() == id {
		t.Fatal("NewID must be random")
	}
}

func TestEnqueueLookupCompleteLifecycle(t *testing.T) {
	q := NewQueue(filepath.Join(t.TempDir(), QueueDirName))
	id := NewID()
	if _, err := q.Lookup(id); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("unknown request lookup = %v", err)
	}
	if err := q.Enqueue(submit(id, "#Test", 1), 10); err != nil {
		t.Fatal(err)
	}
	st, err := q.Lookup(id)
	if err != nil || st.Status != RequestQueued {
		t.Fatalf("queued lookup = %+v, %v", st, err)
	}
	pending, err := q.Pending()
	if err != nil || len(pending) != 1 || pending[0].Err != nil || pending[0].Command.Name != "#Test" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	p := &Proposal{ID: id, Name: "#Test", Status: StatusPending, CreatedAt: 1}
	if err := q.Complete(Result{RequestID: id, Status: RequestPending, Proposal: p, CompletedAt: 2}); err != nil {
		t.Fatal(err)
	}
	st, err = q.Lookup(id)
	if err != nil || st.Status != RequestPending || st.Proposal == nil || st.Proposal.Name != "#Test" {
		t.Fatalf("completed lookup = %+v, %v", st, err)
	}
	if pending, _ := q.Pending(); len(pending) != 0 {
		t.Fatalf("command file not removed: %+v", pending)
	}
	// Completing again (crash replay) is harmless.
	if err := q.Complete(Result{RequestID: id, Status: RequestPending, Proposal: p, CompletedAt: 3}); err != nil {
		t.Fatalf("second complete: %v", err)
	}
}

func TestEnqueueBoundedAndValidated(t *testing.T) {
	q := NewQueue(filepath.Join(t.TempDir(), QueueDirName))
	for i := 0; i < 3; i++ {
		if err := q.Enqueue(submit(NewID(), "#c", int64(i)), 3); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := q.Enqueue(submit(NewID(), "#c", 9), 3); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("4th enqueue = %v, want ErrQueueFull", err)
	}
	if err := q.Enqueue(submit("nothex", "#c", 1), 10); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("bad id = %v", err)
	}
	if err := q.Enqueue(Command{RequestID: NewID(), Op: OpApprove, ProposalID: "x"}, 10); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("bad proposal id = %v", err)
	}
	if err := q.Enqueue(Command{RequestID: NewID(), Op: "drop"}, 10); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("bad op = %v", err)
	}
	// No temp files survive a successful write.
	entries, _ := os.ReadDir(q.Dir())
	for _, e := range entries {
		if e.Name()[0] == '.' {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestPendingOrderAndMalformed(t *testing.T) {
	q := NewQueue(filepath.Join(t.TempDir(), QueueDirName))
	a, b := "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	if err := q.Enqueue(submit(b, "#second", 20), 10); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(submit(a, "#first", 10), 10); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(q.Dir(), "cmd-cccccccccccccccc.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	pending, err := q.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 || pending[0].Err == nil {
		t.Fatalf("malformed file should sort first (createdAt 0) with an error: %+v", pending)
	}
	if pending[1].Command.RequestID != a || pending[2].Command.RequestID != b {
		t.Fatalf("order = %s, %s", pending[1].Command.RequestID, pending[2].Command.RequestID)
	}
	if err := q.Discard(pending[0].Path); err != nil {
		t.Fatal(err)
	}
	if err := q.Discard(filepath.Join(t.TempDir(), "cmd-x.json")); err == nil {
		t.Fatal("Discard outside the queue dir must fail")
	}
}

func TestPruneTTLCapAndTempFiles(t *testing.T) {
	q := NewQueue(filepath.Join(t.TempDir(), QueueDirName))
	now := time.Now()
	ids := []string{"1111111111111111", "2222222222222222", "3333333333333333", "4444444444444444"}
	for i, id := range ids {
		if err := q.Complete(Result{RequestID: id, Status: RequestPending, CompletedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
		// Oldest first: 30h, 3h, 2h, 1h ago.
		age := time.Duration(len(ids)-i) * time.Hour
		if i == 0 {
			age = 30 * time.Hour
		}
		p := filepath.Join(q.Dir(), resultPrefix+id+fileSuffix)
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(q.Dir(), tmpPrefix+"orphan")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	removed, err := q.Prune(now, 24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 1 expired + 1 over the cap of 2 + 1 orphaned temp file.
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}
	for _, id := range ids[:2] {
		if _, err := q.Lookup(id); !errors.Is(err, ErrUnknownRequest) {
			t.Errorf("%s should be pruned, lookup = %v", id, err)
		}
	}
	for _, id := range ids[2:] {
		if _, err := q.Lookup(id); err != nil {
			t.Errorf("%s should be kept, lookup = %v", id, err)
		}
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("orphaned temp file not removed")
	}
}
