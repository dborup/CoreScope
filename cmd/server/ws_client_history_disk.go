package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	wsHistoryFileName     = "ws-client-history.jsonl"
	wsHistoryMaxFileBytes = 8 << 20
	wsHistoryCompactEvery = 24 * 60
)

// One server owns this journal. Only its minute sampler writes it, never a
// connection callback or HTTP request. Disk I/O never holds the ring/Hub locks.
type wsHistoryJournal struct {
	mu               sync.Mutex
	path, mainDBPath string
	appended         int
	failed           bool
	closed           bool
	unlock           func() error
}

func wsHistoryPersistencePath(flagPath, dbPath string) string {
	if flagPath == "off" {
		return ""
	}
	if flagPath != "" {
		return flagPath
	}
	return filepath.Join(filepath.Dir(dbPath), wsHistoryFileName)
}

func (h *wsClientHistory) persistenceStatus() WSHistoryPersistence {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.persistence
}

func (h *wsClientHistory) setPersistenceHealthy(healthy bool) {
	h.mu.Lock()
	h.persistence.Healthy = healthy
	h.mu.Unlock()
}

func checkWSHistoryPath(path, mainDBPath string) error {
	if mainDBPath != "" {
		collision, err := pingScoreHistoryPathsCollide(mainDBPath, path)
		if err != nil {
			return err
		}
		if collision {
			return fmt.Errorf("WebSocket history must not use the packet database file")
		}
	}
	return nil
}

// enablePersistence runs once, before sampling and listening. A bad file is
// preserved byte-for-byte and never retried/overwritten behind the operator's
// back. The server remains usable with visibly degraded, RAM-only history.
func (h *wsClientHistory) enablePersistence(path, mainDBPath string, now time.Time) error {
	if path == "" {
		return nil
	}
	h.mu.Lock()
	h.persistence.Enabled = true
	h.mu.Unlock()
	if err := checkWSHistoryPath(path, mainDBPath); err != nil {
		return err
	}
	if err := checkWSHistoryPath(path+".lock", mainDBPath); err != nil {
		return err
	}
	unlock, err := lockWSHistoryFile(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock WebSocket history: %w", err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			_ = unlock()
		}
	}()
	samples, err := readWSHistoryJournal(path, now)
	if err != nil {
		return err
	}
	for _, s := range samples {
		h.sample(s.Clients, s.T)
		h.mu.Lock()
		h.buf[(h.head-1+len(h.buf))%len(h.buf)] = s
		h.mu.Unlock()
	}
	h.mu.Lock()
	h.peak = 0 // a previous process's peak is never a live peak
	h.mu.Unlock()
	if len(samples) > 0 {
		h.markRestartGap()
	}
	j := &wsHistoryJournal{path: path, mainDBPath: mainDBPath, unlock: unlock}
	if err := j.compact(h.snapshot(), now); err != nil {
		return err
	}
	h.journal = j
	keepLock = true
	h.setPersistenceHealthy(true)
	return nil
}

func (h *wsClientHistory) closePersistence() {
	j := h.journal
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	j.closed = true
	if err := j.unlock(); err != nil {
		log.Printf("[ws-history] close journal lock: %v", err)
	}
}

func readWSHistoryJournal(path string, now time.Time) ([]WSClientSample, error) {
	f, err := openWSHistoryFile(path, os.O_RDONLY)
	if os.IsNotExist(err) {
		return []WSClientSample{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open WebSocket history: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > wsHistoryMaxFileBytes {
		return nil, fmt.Errorf("WebSocket history exceeds 8 MiB limit; original preserved")
	}
	limited := &io.LimitedReader{R: f, N: wsHistoryMaxFileBytes + 1}
	reader := bufio.NewReaderSize(limited, 4096)
	tail := newWSClientHistory(wsHistoryCapacity)
	var last time.Time
	validRecords := 0
	for {
		line, readErr := reader.ReadSlice('\n')
		if limited.N <= 0 {
			return nil, fmt.Errorf("WebSocket history exceeds 8 MiB limit; original preserved")
		}
		if readErr == io.EOF {
			// One write includes its newline. An unterminated final record
			// is a torn append, not part of the preceding synced history.
			if len(line) > 0 {
				if validRecords == 0 {
					return nil, fmt.Errorf("unrecognized unterminated WebSocket history; original preserved")
				}
				log.Printf("[ws-history] recovering saved history; discarded incomplete final record")
			}
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read WebSocket history; original preserved: %w", readErr)
		}
		var s WSClientSample
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return nil, fmt.Errorf("invalid WebSocket history record; original preserved: %w", err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("trailing WebSocket history data; original preserved")
		}
		if s.T.IsZero() || (!last.IsZero() && !s.T.After(last)) || s.Clients < 0 || s.Peak < s.Clients {
			return nil, fmt.Errorf("invalid WebSocket history values/order; original preserved")
		}
		last = s.T
		validRecords++
		if s.T.Before(now.Add(-wsHistoryRetention)) {
			continue
		}
		// A record dated after "now" (e.g. a backward NTP correction applied
		// since it was written) is neither corrupt nor out of order — it is
		// still kept here and re-saved by the next compaction exactly like a
		// record within the compaction loop's own retention window below.
		// snapshotRange already excludes anything after its request's "now",
		// so the point simply reappears once wall time catches up to it
		// instead of disabling persistence for the rest of the journal.
		tail.sample(s.Clients, s.T)
		tail.buf[(tail.head-1+len(tail.buf))%len(tail.buf)] = s
	}
	return tail.snapshot(), nil
}

func (h *wsClientHistory) sampleAndPersist(current int, at time.Time) {
	s := h.sample(current, at)
	if s.T.IsZero() {
		return
	}
	j := h.journal
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failed || j.closed {
		return
	}
	var err error
	if j.appended >= wsHistoryCompactEvery {
		err = j.compact(h.snapshot(), at)
	} else {
		err = j.append(s)
	}
	if err != nil {
		// A partial append cannot safely be followed by another append. Stop
		// writes until restart/operator recovery; log once, not every minute.
		j.failed = true
		h.setPersistenceHealthy(false)
		log.Printf("[ws-history] persistence stopped; RAM history remains available: %v", err)
	}
}

func (j *wsHistoryJournal) append(s WSClientSample) error {
	if err := checkWSHistoryPath(j.path, j.mainDBPath); err != nil {
		return err
	}
	f, err := openWSHistoryFile(j.path, os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if st.Size()+int64(len(data)) > wsHistoryMaxFileBytes {
		return fmt.Errorf("WebSocket history exceeds 8 MiB limit")
	}
	if n, err := f.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	j.appended++
	return nil
}

// Compact in the same directory so rename is atomic. Never truncate a live
// file in place: a crash before rename leaves the preceding journal intact.
func (j *wsHistoryJournal) compact(samples []WSClientSample, now time.Time) error {
	if err := checkWSHistoryPath(j.path, j.mainDBPath); err != nil {
		return err
	}
	if f, err := openWSHistoryFile(j.path, os.O_RDONLY); err == nil {
		f.Close()
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(j.path), ".ws-client-history-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, s := range samples {
		// A small backward clock adjustment can put the last saved point
		// slightly in the future. Preserve it on disk; the API excludes it
		// until wall time catches up, rather than silently deleting it.
		if s.T.Before(now.Add(-wsHistoryRetention)) {
			continue
		}
		if err := enc.Encode(s); err != nil {
			return err
		}
	}
	if st, err := f.Stat(); err != nil {
		return err
	} else if st.Size() > wsHistoryMaxFileBytes {
		return fmt.Errorf("compacted WebSocket history exceeds 8 MiB limit")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := checkWSHistoryPath(j.path, j.mainDBPath); err != nil {
		return err
	}
	if err := os.Rename(tmp, j.path); err != nil {
		return err
	}
	// Persist the new directory entry as well as the file's contents.
	dir, err := os.Open(filepath.Dir(j.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	j.appended = 0
	return nil
}
