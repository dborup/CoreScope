package main

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/meshcore-analyzer/channelregistry"
	"github.com/meshcore-analyzer/infrastructure"
)

// runInfrastructureQueueOnce is called by the ingestor only. The server never
// writes the selected snapshot; it writes atomic queue commands only.
func runInfrastructureQueueOnce(store *Store) error {
	q := channelregistry.NewQueue(infrastructure.QueuePath(store.path))
	commands, err := q.Pending()
	if err != nil {
		return err
	}
	state, err := infrastructure.LoadDB(store.db)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, queued := range commands {
		if queued.Err != nil {
			log.Printf("[infrastructure] unreadable command: %v", queued.Err)
			if err := q.Discard(queued.Path); err != nil {
				return err
			}
			continue
		}
		cmd := queued.Command
		res := channelregistry.Result{RequestID: cmd.RequestID, CompletedAt: now}
		op, key := "", ""
		switch cmd.Op {
		case channelregistry.OpSubmit:
			op, key = infrastructure.OpSelect, cmd.Name
		case channelregistry.OpRevoke:
			op, key = infrastructure.OpRemove, cmd.ProposalID
		}
		if cmd.CreatedAt <= 0 || now-cmd.CreatedAt > 5*60*1000 || cmd.CreatedAt > now+60*1000 {
			res.Status, res.Error = channelregistry.RequestError, "request expired"
		} else if op == "" {
			res.Status, res.Error = channelregistry.RequestError, "invalid operation"
		} else if op == infrastructure.OpSelect && !knownInfrastructureRepeater(store, key) {
			res.Status, res.Error = channelregistry.RequestError, "node is not a known repeater"
		} else if err := state.Apply(cmd.RequestID, op, key, now); err != nil {
			res.Status, res.Error = channelregistry.RequestError, err.Error()
		} else {
			// The SQLite row is the durable idempotence record as well as
			// the selected set. Commit it before writing the result file.
			if err := infrastructure.SaveDB(store.db, state); err != nil {
				return fmt.Errorf("save infrastructure state: %w", err)
			}
			if op == infrastructure.OpSelect {
				res.Status = channelregistry.RequestApproved
			} else {
				res.Status = channelregistry.RequestRevoked
			}
		}
		if err := q.Complete(res); err != nil {
			return fmt.Errorf("complete infrastructure request: %w", err)
		}
	}
	if _, err := q.Prune(time.Now(), 10*time.Minute, infrastructure.MaxRecent); err != nil && !errors.Is(err, channelregistry.ErrUnknownRequest) {
		return err
	}
	return nil
}

func knownInfrastructureRepeater(store *Store, key string) bool {
	known, err := infrastructure.KnownRepeater(store.db, key)
	if err != nil {
		log.Printf("[infrastructure] repeater lookup failed: %v", err)
	}
	return err == nil && known
}
