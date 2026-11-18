//go:build unix

package channelregistry

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// #909: a queued command can carry the admin-only owner contact (#898), so
// the queue must be readable only by the account that runs the server and
// the ingestor: files 0600, the directory 0700, whatever the umask.

// zeroUmask clears the umask for the test, so a mode that only looks right
// because of the default 022 umask still fails.
func zeroUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s: mode %04o, want %04o", filepath.Base(path), got, want)
	}
}

func ownerOf(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return st.Uid, st.Gid
}

func TestQueue909_NewFilesAndDirArePrivate(t *testing.T) {
	zeroUmask(t)
	q := NewQueue(filepath.Join(t.TempDir(), QueueDirName))
	id := NewID()
	if err := q.Enqueue(submit(id, "#Test", 1), 10); err != nil {
		t.Fatal(err)
	}
	assertMode(t, q.Dir(), 0o700)
	assertMode(t, q.cmdPath(id), 0o600)

	if err := q.Complete(Result{RequestID: id, Status: RequestApproved, CompletedAt: 2}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, q.resultPath(id), 0o600)

	if err := q.WriteBuiltinNames([]string{"#a"}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, q.BuiltinNamesPath(), 0o600)
}

// Each writer creates the directory 0700 when it is the first to need it.
func TestQueue909_EveryWriterCreatesPrivateDir(t *testing.T) {
	zeroUmask(t)
	id := NewID()
	for name, write := range map[string]func(q *Queue) error{
		"EnqueueValue":      func(q *Queue) error { return q.EnqueueValue(id, map[string]any{"requestId": id, "createdAt": 1}, 10) },
		"CompleteValue":     func(q *Queue) error { return q.CompleteValue(id, map[string]any{"requestId": id}) },
		"WriteBuiltinNames": func(q *Queue) error { return q.WriteBuiltinNames([]string{"#a"}) },
	} {
		t.Run(name, func(t *testing.T) {
			q := NewQueue(filepath.Join(t.TempDir(), "q"))
			if err := write(q); err != nil {
				t.Fatal(err)
			}
			assertMode(t, q.Dir(), 0o700)
		})
	}
}

// A directory made 0755 before #909 is tightened by the next write and by
// Prune, which the ingestor runs at startup; its owner never changes.
func TestQueue909_ExistingDirIsTightened(t *testing.T) {
	zeroUmask(t)
	id := NewID()
	for name, touch := range map[string]func(q *Queue) error{
		"Enqueue":           func(q *Queue) error { return q.Enqueue(submit(id, "#Test", 1), 10) },
		"CompleteValue":     func(q *Queue) error { return q.CompleteValue(id, map[string]any{"requestId": id}) },
		"WriteBuiltinNames": func(q *Queue) error { return q.WriteBuiltinNames([]string{"#a"}) },
		"Prune": func(q *Queue) error {
			_, err := q.Prune(time.Now(), time.Hour, 10)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			q := NewQueue(filepath.Join(t.TempDir(), "q"))
			if err := os.Mkdir(q.Dir(), 0o755); err != nil {
				t.Fatal(err)
			}
			uid, gid := ownerOf(t, q.Dir())
			if err := touch(q); err != nil {
				t.Fatal(err)
			}
			assertMode(t, q.Dir(), 0o700)
			if u, g := ownerOf(t, q.Dir()); u != uid || g != gid {
				t.Errorf("owner changed: %d:%d -> %d:%d", uid, gid, u, g)
			}
		})
	}
}

// Prune on a queue nobody has written yet must not create the directory.
func TestQueue909_PruneDoesNotCreateDir(t *testing.T) {
	q := NewQueue(filepath.Join(t.TempDir(), "q"))
	if _, err := q.Prune(time.Now(), time.Hour, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(q.Dir()); !os.IsNotExist(err) {
		t.Fatalf("Prune created the queue directory: %v", err)
	}
}
