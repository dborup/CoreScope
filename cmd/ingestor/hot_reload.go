package main

import (
	"bytes"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/meshcore-analyzer/regions"
)

// hotKeys holds the channel-decryption and region-scope keys behind atomic
// pointers so a SIGHUP can safely swap in freshly-loaded values while MQTT
// message handlers are concurrently reading the current ones. Neither
// loadChannelKeys nor loadRegionKeys re-runs automatically otherwise —
// hashChannels/hashRegions additions in config.json require a full ingestor
// restart to take effect without this.
//
// Channel keys come in two layers. base is what loadChannelKeys derives from
// config (builtin, rainbow, hashChannels, channelKeys) and is replaced on
// every SIGHUP. approved holds the publicly suggested hashtag channels an
// administrator approved (internal/channelregistry): approvals add to it and
// revocations remove from it (RemoveApproved), it is reloaded from the
// database at startup, and a SIGHUP never touches it, so a reload can neither
// drop an approved channel nor bring back a revoked one. channelKeys is the
// merged snapshot the MQTT handlers read, with base winning over approved, so
// a manually configured key for the same name keeps priority. It also means
// a name already in base (for example one of the ~320 rainbow-table names
// such as #test or #chat) gains nothing from approval and keeps decrypting
// after a revoke; BaseNames lets the proposal runner tell the server so.
//
// Every change that takes a key out of an effective set — a revoke, a reload
// without the entry, or a reload that gives a name a new value — is reported
// to the onRemoved hook after the lock is released (#380: the decode backfill
// forgets the key on the event itself, so a quick re-approval cannot hide it).
type hotKeys struct {
	channelKeys atomic.Pointer[map[string]string]
	regionKeys  atomic.Pointer[map[string][]byte]
	removedHook atomic.Pointer[func([]removedKey)]

	mu              sync.Mutex // serializes writers of base/approved and the merge
	base            map[string]string
	approved        map[string]string
	baseRegions     map[string][]byte
	approvedRegions map[string][]byte
	baseGen         uint64 // incremented by every setBase
}

// newHotKeys wraps the initial startup-loaded key maps.
func newHotKeys(channelKeys map[string]string, regionKeys map[string][]byte) *hotKeys {
	hk := &hotKeys{approved: make(map[string]string), approvedRegions: make(map[string][]byte)}
	hk.setBaseRegions(regionKeys)
	hk.setBase(channelKeys)
	return hk
}

// onRemoved sets the hook that receives every key leaving an effective set.
// It runs synchronously in the goroutine that made the change, outside hk.mu,
// so it may write to the database; callers must not hold a transaction.
func (hk *hotKeys) onRemoved(fn func([]removedKey)) {
	hk.removedHook.Store(&fn)
}

func (hk *hotKeys) reportRemoved(removed []removedKey) {
	if len(removed) == 0 {
		return
	}
	if fn := hk.removedHook.Load(); fn != nil && *fn != nil {
		(*fn)(removed)
	}
}

// setBase replaces the config-derived channel keys and republishes the merge.
func (hk *hotKeys) setBase(channelKeys map[string]string) {
	hk.mu.Lock()
	hk.base = channelKeys
	hk.baseGen++
	removed := hk.publishLocked()
	hk.mu.Unlock()
	hk.reportRemoved(removed)
}

// BaseNames returns the names in the config-derived layer and its generation,
// which changes on every setBase (startup and each reload).
func (hk *hotKeys) BaseNames() ([]string, uint64) {
	hk.mu.Lock()
	defer hk.mu.Unlock()
	names := make([]string, 0, len(hk.base))
	for name := range hk.base {
		names = append(names, name)
	}
	return names, hk.baseGen
}

// AddApproved adds approved hashtag channels, deriving each key with the same
// algorithm as hashChannels. It returns how many names were new. Names
// already approved are ignored, so repeated calls (startup load, replayed
// approvals) are harmless.
func (hk *hotKeys) AddApproved(names ...string) int {
	hk.mu.Lock()
	defer hk.mu.Unlock()
	added := 0
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := hk.approved[name]; ok {
			continue
		}
		hk.approved[name] = deriveHashtagChannelKey(name)
		added++
	}
	if added > 0 {
		hk.publishLocked()
	}
	return added
}

// RemoveApproved removes a name from the approved layer only (never touches
// base, so a manually configured key for the same channel keeps decrypting
// it). Returns whether it was present.
func (hk *hotKeys) RemoveApproved(name string) bool {
	hk.mu.Lock()
	if _, ok := hk.approved[name]; !ok {
		hk.mu.Unlock()
		return false
	}
	delete(hk.approved, name)
	removed := hk.publishLocked()
	hk.mu.Unlock()
	hk.reportRemoved(removed)
	return true
}

// publishLocked builds a fresh merged map (never mutating one a reader may
// hold) and swaps it in, returning the keys of the previous merge that are
// gone or now carry a different value. O(base + approved), and only on
// reload/approval.
func (hk *hotKeys) publishLocked() []removedKey {
	merged := make(map[string]string, len(hk.base)+len(hk.approved))
	for name, key := range hk.approved {
		merged[name] = key
	}
	for name, key := range hk.base {
		merged[name] = key
	}
	var removed []removedKey
	if prev := hk.channelKeys.Load(); prev != nil {
		for name, key := range *prev {
			if nk, ok := merged[name]; !ok || !sameChannelKey(nk, key) {
				removed = append(removed, removedKey{decodeBackfillKindChannel, name, channelKeyBytes(key)})
			}
		}
	}
	hk.channelKeys.Store(&merged)
	return removed
}

// Channels returns the current channel-decryption key snapshot. Safe to
// call from any goroutine; cheap (one atomic load, no lock).
func (hk *hotKeys) Channels() map[string]string {
	return *hk.channelKeys.Load()
}

// Regions returns the current region-scope key snapshot. Safe to call from
// any goroutine; cheap (one atomic load, no lock).
func (hk *hotKeys) Regions() map[string][]byte {
	return *hk.regionKeys.Load()
}

// Region layers mirror channel layers: config (including hashRegionsPath)
// wins over approved names. A published map is immutable for concurrent reads.
func (hk *hotKeys) publishRegionsLocked() []removedKey {
	merged := make(map[string][]byte, len(hk.baseRegions)+len(hk.approvedRegions))
	for name, key := range hk.approvedRegions {
		merged[name] = key
	}
	for name, key := range hk.baseRegions {
		merged[name] = key
	}
	var removed []removedKey
	if prev := hk.regionKeys.Load(); prev != nil {
		for name, key := range *prev {
			if nk, ok := merged[name]; !ok || !bytes.Equal(nk, key) {
				removed = append(removed, removedKey{decodeBackfillKindScope, name, key})
			}
		}
	}
	hk.regionKeys.Store(&merged)
	return removed
}

func (hk *hotKeys) setBaseRegions(base map[string][]byte) {
	hk.mu.Lock()
	hk.baseRegions = base
	removed := hk.publishRegionsLocked()
	hk.mu.Unlock()
	hk.reportRemoved(removed)
}

func (hk *hotKeys) AddApprovedRegions(names ...string) int {
	hk.mu.Lock()
	defer hk.mu.Unlock()
	added := 0
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, exists := hk.approvedRegions[name]; exists {
			continue
		}
		hk.approvedRegions[name] = regions.Key(name)
		added++
	}
	if added > 0 {
		hk.publishRegionsLocked()
	}
	return added
}

// IsConfiguredRegion reports whether name is in the config layer
// (hashRegions or hashRegionsPath), as last loaded or reloaded.
func (hk *hotKeys) IsConfiguredRegion(name string) bool {
	hk.mu.Lock()
	defer hk.mu.Unlock()
	_, ok := hk.baseRegions[name]
	return ok
}

// ApprovedRegionCount returns the size of the admin-approved region layer.
func (hk *hotKeys) ApprovedRegionCount() int {
	hk.mu.Lock()
	defer hk.mu.Unlock()
	return len(hk.approvedRegions)
}

func (hk *hotKeys) RemoveApprovedRegion(name string) bool {
	hk.mu.Lock()
	if _, ok := hk.approvedRegions[name]; !ok {
		hk.mu.Unlock()
		return false
	}
	delete(hk.approvedRegions, name)
	removed := hk.publishRegionsLocked()
	hk.mu.Unlock()
	hk.reportRemoved(removed)
	return true
}

// reload re-reads configPath from disk — and the external hashRegionsPath
// file it names (#360) — and atomically swaps in freshly derived
// channel/region keys. On any error the previous keys are left in place: a
// malformed config.json, or a malformed region-name file, during a live
// reload must not blank out a working ingestor.
func (hk *hotKeys) reload(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	ck := loadChannelKeys(cfg, configPath)
	rk, err := loadRegionKeys(cfg, configPath)
	if err != nil {
		// A SIGHUP re-reads hashRegionsPath too, and an unusable file must
		// not swap in a shrunken region set. Bail before any store, so the
		// caller logs and every previous key stays live.
		return err
	}
	hk.setBase(ck)
	hk.setBaseRegions(rk)
	log.Printf("[hot-reload] reloaded %d channel key(s), %d region key(s) from %s (approved shared channels and %d approved region scope(s) kept)", len(ck), len(rk), configPath, hk.ApprovedRegionCount())
	return nil
}

// startSIGHUPReload spawns a goroutine that reloads hashChannels/hashRegions
// (via hk.reload) every time the process receives SIGHUP — the standard
// Unix "reload config without restart" convention:
//
//	kill -HUP <ingestor-pid>
//
// Returns a stop func that unregisters the signal and lets the goroutine
// exit; safe to defer from main().
func startSIGHUPReload(hk *hotKeys, configPath string) (stop func()) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sighup:
				log.Printf("[hot-reload] SIGHUP received, reloading hashChannels/hashRegions from %s", configPath)
				if err := hk.reload(configPath); err != nil {
					log.Printf("[hot-reload] reload failed, keeping previous keys: %v", err)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sighup)
		close(done)
	}
}
