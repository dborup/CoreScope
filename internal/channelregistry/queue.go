package channelregistry

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// QueueDirName is the directory, next to the SQLite database, that holds the
// command and result files. Keeping it beside the database gives it the same
// volume, backup and permission story as the prune queue (internal/prunequeue).
//
// Layout:
//
//	cmd-<requestId>.json     written by the server, one per request
//	result-<requestId>.json  written by the ingestor after applying it; the
//	                         ingestor then removes the command file
//	.tmp-*                   in-flight writes, renamed into place atomically
const QueueDirName = "channel-proposal-requests"

const (
	cmdPrefix    = "cmd-"
	resultPrefix = "result-"
	tmpPrefix    = ".tmp-"
	fileSuffix   = ".json"

	// staleTmpAge is how old an orphaned temp file (from a writer that died
	// between create and rename) must be before Prune removes it.
	staleTmpAge = time.Hour
)

// Queue errors.
var (
	ErrQueueFull      = errors.New("the suggestion queue is full, please try again later")
	ErrInvalidID      = errors.New("invalid request id")
	ErrInvalidCommand = errors.New("invalid command")
	ErrUnknownRequest = errors.New("unknown or expired request")
)

// QueueDir returns the queue directory for a database path.
func QueueDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), QueueDirName)
}

// NewID returns a random 16-hex-character id. Ids double as file names and as
// proposal ids, so they are random rather than time-based.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ValidID accepts lowercase hex ids of 16 to 64 characters. Anything else is
// rejected before it can reach a file path.
func ValidID(id string) bool {
	if len(id) < 16 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Queue is the file queue in one directory. The mutex only serializes callers
// inside one process; the server and the ingestor coordinate through atomic
// renames, never through shared locks.
type Queue struct {
	dir string
	mu  sync.Mutex
}

// NewQueue returns a queue rooted at dir. The directory is created on first
// write.
func NewQueue(dir string) *Queue {
	return &Queue{dir: dir}
}

// Dir returns the queue directory.
func (q *Queue) Dir() string { return q.dir }

// QueuedCommand is one command file. Err is set when the file could not be
// read or parsed; the ingestor discards such files.
type QueuedCommand struct {
	Path    string
	Command Command
	Err     error
}

func (q *Queue) cmdPath(id string) string {
	return filepath.Join(q.dir, cmdPrefix+id+fileSuffix)
}

func (q *Queue) resultPath(id string) string {
	return filepath.Join(q.dir, resultPrefix+id+fileSuffix)
}

// Enqueue writes cmd atomically, refusing when maxQueued commands are already
// waiting.
func (q *Queue) Enqueue(cmd Command, maxQueued int) error {
	if !ValidID(cmd.RequestID) {
		return ErrInvalidID
	}
	switch cmd.Op {
	case OpSubmit:
		if cmd.Name == "" {
			return ErrInvalidCommand
		}
	case OpApprove, OpReject, OpRevoke:
		if !ValidID(cmd.ProposalID) {
			return ErrInvalidCommand
		}
	default:
		return ErrInvalidCommand
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := os.MkdirAll(q.dir, 0o755); err != nil {
		return err
	}
	names, err := q.listNames(cmdPrefix)
	if err != nil {
		return err
	}
	if len(names) >= maxQueued {
		return ErrQueueFull
	}
	return q.writeAtomic(q.cmdPath(cmd.RequestID), cmd)
}

// Pending returns the waiting commands in submission order (createdAt, then
// request id).
func (q *Queue) Pending() ([]QueuedCommand, error) {
	names, err := q.listNames(cmdPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]QueuedCommand, 0, len(names))
	for _, name := range names {
		p := filepath.Join(q.dir, name)
		qc := QueuedCommand{Path: p}
		b, err := os.ReadFile(p)
		switch {
		case err != nil:
			qc.Err = err
		case json.Unmarshal(b, &qc.Command) != nil:
			qc.Err = ErrInvalidCommand
		case !ValidID(qc.Command.RequestID) || p != q.cmdPath(qc.Command.RequestID):
			qc.Err = ErrInvalidID
		}
		out = append(out, qc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Command, out[j].Command
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		return a.RequestID < b.RequestID
	})
	return out, nil
}

// Complete records the outcome of a command, then removes the command file.
// The result is written first: a crash between the two steps leaves both
// files, and the command is simply applied again, which the ingestor makes
// idempotent.
func (q *Queue) Complete(res Result) error {
	if !ValidID(res.RequestID) {
		return ErrInvalidID
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := os.MkdirAll(q.dir, 0o755); err != nil {
		return err
	}
	if err := q.writeAtomic(q.resultPath(res.RequestID), res); err != nil {
		return err
	}
	if err := os.Remove(q.cmdPath(res.RequestID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Discard removes a command file that cannot be processed.
func (q *Queue) Discard(path string) error {
	if filepath.Dir(path) != filepath.Clean(q.dir) || !strings.HasPrefix(filepath.Base(path), cmdPrefix) {
		return ErrInvalidCommand
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Lookup returns the public state of a request.
func (q *Queue) Lookup(id string) (RequestStatus, error) {
	if !ValidID(id) {
		return RequestStatus{}, ErrInvalidID
	}
	b, err := os.ReadFile(q.resultPath(id))
	if err == nil {
		var res Result
		if err := json.Unmarshal(b, &res); err != nil {
			return RequestStatus{}, err
		}
		return RequestStatus{Status: res.Status, Proposal: res.Proposal, Error: res.Error, CompletedAt: res.CompletedAt}, nil
	}
	if !os.IsNotExist(err) {
		return RequestStatus{}, err
	}
	if _, err := os.Stat(q.cmdPath(id)); err == nil {
		return RequestStatus{Status: RequestQueued}, nil
	} else if !os.IsNotExist(err) {
		return RequestStatus{}, err
	}
	return RequestStatus{}, ErrUnknownRequest
}

// Prune removes result files older than ttl, the oldest results beyond
// maxResults, and orphaned temp files. Commands are never pruned here: the
// ingestor answers expired commands with an error result instead, so a
// polling client always learns what happened.
func (q *Queue) Prune(now time.Time, ttl time.Duration, maxResults int) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	type aged struct {
		path string
		mod  time.Time
	}
	var results []aged
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(q.dir, e.Name())
		switch {
		case strings.HasPrefix(e.Name(), tmpPrefix):
			if now.Sub(info.ModTime()) > staleTmpAge && os.Remove(p) == nil {
				removed++
			}
		case strings.HasPrefix(e.Name(), resultPrefix) && strings.HasSuffix(e.Name(), fileSuffix):
			if now.Sub(info.ModTime()) > ttl {
				if os.Remove(p) == nil {
					removed++
				}
				continue
			}
			results = append(results, aged{p, info.ModTime()})
		}
	}
	if maxResults > 0 && len(results) > maxResults {
		sort.Slice(results, func(i, j int) bool { return results[i].mod.Before(results[j].mod) })
		for _, r := range results[:len(results)-maxResults] {
			if os.Remove(r.path) == nil {
				removed++
			}
		}
	}
	return removed, nil
}

func (q *Queue) listNames(prefix string) ([]string, error) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); strings.HasPrefix(name, prefix) && strings.HasSuffix(name, fileSuffix) {
			out = append(out, name)
		}
	}
	return out, nil
}

// writeAtomic writes v as JSON to a temp file in the same directory, syncs
// it, renames it into place and syncs the directory, so a reader never sees a
// partial file and a crash never leaves a truncated one under the final name.
func (q *Queue) writeAtomic(path string, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(q.dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir fsyncs a directory so a rename into it survives a power loss, not
// just a process crash. Best effort: some filesystems and platforms refuse to
// sync a directory, and the file itself is already synced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}
