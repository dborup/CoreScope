package channelregistry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// otherCmd is a command type the queue knows nothing about (#789 M2a: the
// antenna claim queue reuses this file queue with its own types).
type otherCmd struct {
	RequestID string  `json:"requestId"`
	Op        string  `json:"op"`
	HeightM   float64 `json:"heightM"`
	CreatedAt int64   `json:"createdAt"`
}

type otherResult struct {
	RequestID string `json:"requestId"`
	Status    string `json:"status"`
}

func TestQueueValue789_Lifecycle(t *testing.T) {
	q := NewQueue(filepath.Join(t.TempDir(), "antenna-claim-requests"))
	a, b := "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	var res otherResult
	if _, err := q.LookupValue(a, &res); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("unknown lookup = %v", err)
	}
	// Submission order is createdAt, not file name.
	if err := q.EnqueueValue(b, otherCmd{RequestID: b, Op: "claim", HeightM: 2, CreatedAt: 1}, 10); err != nil {
		t.Fatal(err)
	}
	if err := q.EnqueueValue(a, otherCmd{RequestID: a, Op: "claim", HeightM: 1, CreatedAt: 2}, 10); err != nil {
		t.Fatal(err)
	}
	if queued, err := q.LookupValue(a, &res); err != nil || !queued {
		t.Fatalf("queued lookup = %v, %v", queued, err)
	}
	raw, err := q.PendingValues()
	if err != nil || len(raw) != 2 || raw[0].RequestID != b || raw[1].RequestID != a {
		t.Fatalf("pending = %+v, %v", raw, err)
	}
	var c otherCmd
	if err := json.Unmarshal(raw[1].Data, &c); err != nil || c.HeightM != 1 {
		t.Fatalf("data = %s, %v", raw[1].Data, err)
	}
	if err := q.CompleteValue(a, otherResult{RequestID: a, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if queued, err := q.LookupValue(a, &res); err != nil || queued || res.Status != "pending" {
		t.Fatalf("completed lookup = %v %+v %v", queued, res, err)
	}
	if raw, _ := q.PendingValues(); len(raw) != 1 || raw[0].RequestID != b {
		t.Fatalf("command not removed: %+v", raw)
	}
}

func TestQueueValue789_Bounds(t *testing.T) {
	q := NewQueue(filepath.Join(t.TempDir(), "antenna-claim-requests"))
	if err := q.EnqueueValue("../../etc/passwd", otherCmd{}, 10); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("bad id = %v", err)
	}
	for i, id := range []string{"1111111111111111", "2222222222222222"} {
		if err := q.EnqueueValue(id, otherCmd{RequestID: id, CreatedAt: int64(i)}, 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.EnqueueValue("3333333333333333", otherCmd{RequestID: "3333333333333333"}, 2); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full queue = %v", err)
	}
}

func TestQueueValue789_RejectsMismatchedAndBroken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "antenna-claim-requests")
	q := NewQueue(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file whose requestId does not match its name, and one that is not JSON.
	os.WriteFile(filepath.Join(dir, "cmd-aaaaaaaaaaaaaaaa.json"), []byte(`{"requestId":"bbbbbbbbbbbbbbbb"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "cmd-cccccccccccccccc.json"), []byte(`{`), 0o644)
	raw, err := q.PendingValues()
	if err != nil || len(raw) != 2 {
		t.Fatalf("pending = %+v, %v", raw, err)
	}
	for _, r := range raw {
		if r.Err == nil {
			t.Fatalf("bad file accepted: %+v", r)
		}
	}
}
