package main

import (
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
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
// administrator approved (internal/channelregistry); it only ever grows while
// the process runs and is reloaded from the database at startup, so a SIGHUP
// can never drop an approved channel. channelKeys is the merged snapshot the
// MQTT handlers read, with base winning over approved, so a manually
// configured key for the same name keeps priority.
type hotKeys struct {
	channelKeys atomic.Pointer[map[string]string]
	regionKeys  atomic.Pointer[map[string][]byte]

	mu       sync.Mutex // serializes writers of base/approved and the merge
	base     map[string]string
	approved map[string]string
}

// newHotKeys wraps the initial startup-loaded key maps.
func newHotKeys(channelKeys map[string]string, regionKeys map[string][]byte) *hotKeys {
	hk := &hotKeys{approved: make(map[string]string)}
	hk.regionKeys.Store(&regionKeys)
	hk.setBase(channelKeys)
	return hk
}

// setBase replaces the config-derived channel keys and republishes the merge.
func (hk *hotKeys) setBase(channelKeys map[string]string) {
	hk.mu.Lock()
	defer hk.mu.Unlock()
	hk.base = channelKeys
	hk.publishLocked()
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
	defer hk.mu.Unlock()
	if _, ok := hk.approved[name]; !ok {
		return false
	}
	delete(hk.approved, name)
	hk.publishLocked()
	return true
}

// publishLocked builds a fresh merged map (never mutating one a reader may
// hold) and swaps it in. O(base + approved), and only on reload/approval.
func (hk *hotKeys) publishLocked() {
	merged := make(map[string]string, len(hk.base)+len(hk.approved))
	for name, key := range hk.approved {
		merged[name] = key
	}
	for name, key := range hk.base {
		merged[name] = key
	}
	hk.channelKeys.Store(&merged)
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

// reload re-reads configPath from disk and atomically swaps in freshly
// derived channel/region keys. On any error the previous keys are left in
// place — a malformed config.json during a live reload must not blank out
// a working ingestor.
func (hk *hotKeys) reload(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	ck := loadChannelKeys(cfg, configPath)
	rk := loadRegionKeys(cfg)
	hk.setBase(ck)
	hk.regionKeys.Store(&rk)
	log.Printf("[hot-reload] reloaded %d channel key(s), %d region key(s) from %s (approved shared channels kept)", len(ck), len(rk), configPath)
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
