package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/infrastructure"
)

func TestInfrastructureQueueRoundTripAndReplay(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mesh.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	q := channelregistry.NewQueue(infrastructure.QueuePath(dbPath))
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := store.db.Exec(`INSERT INTO nodes(public_key, role) VALUES(?, 'repeater')`, key); err != nil {
		t.Fatal(err)
	}
	phantom := channelregistry.Command{RequestID: channelregistry.NewID(), Op: channelregistry.OpSubmit, Name: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", CreatedAt: time.Now().UnixMilli()}
	if err := q.Enqueue(phantom, 256); err != nil {
		t.Fatal(err)
	}
	if err := runInfrastructureQueueOnce(store); err != nil {
		t.Fatal(err)
	}
	bad, err := q.Lookup(phantom.RequestID)
	if err != nil || bad.Status != channelregistry.RequestError {
		t.Fatalf("phantom accepted: %+v, %v", bad, err)
	}
	a := channelregistry.Command{RequestID: channelregistry.NewID(), Op: channelregistry.OpSubmit, Name: key, CreatedAt: time.Now().UnixMilli()}
	if err := q.Enqueue(a, 256); err != nil {
		t.Fatal(err)
	}
	if err := runInfrastructureQueueOnce(store); err != nil {
		t.Fatal(err)
	}
	s, err := infrastructure.LoadDB(store.db)
	if err != nil || len(s.Selected) != 1 {
		t.Fatalf("selected = %+v, %v", s.Selected, err)
	}
	b := channelregistry.Command{RequestID: channelregistry.NewID(), Op: channelregistry.OpRevoke, ProposalID: key, CreatedAt: time.Now().UnixMilli()}
	if err := q.Enqueue(b, 256); err != nil {
		t.Fatal(err)
	}
	if err := runInfrastructureQueueOnce(store); err != nil {
		t.Fatal(err)
	}
	// A stale, already-applied command may reappear after a crash. Its durable
	// request ID must not undo the later remove.
	if err := q.Enqueue(a, 256); err != nil {
		t.Fatal(err)
	}
	if err := runInfrastructureQueueOnce(store); err != nil {
		t.Fatal(err)
	}
	s, err = infrastructure.LoadDB(store.db)
	if err != nil || len(s.Selected) != 0 {
		t.Fatalf("replayed old select: %+v, %v", s, err)
	}
}

func TestInfrastructureSelectionSurvivesDatabaseRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := infrastructure.SaveDB(store.db, infrastructure.State{Selected: []infrastructure.Entry{{PublicKey: key, AddedAt: 42}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := infrastructure.LoadDB(reopened.db)
	if err != nil || len(state.Selected) != 1 || state.Selected[0].PublicKey != key {
		t.Fatalf("restart lost curation: %+v, %v", state, err)
	}
}
