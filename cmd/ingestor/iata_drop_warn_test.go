package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Unit tests for the bounded observerIATAWhitelist drop throttle (#110),
// with an explicit clock. iata_drop_log_test.go covers the log output
// through handleMessage.

var t0IATA = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func TestIATAWarnIntervalDefaultAndConfigured(t *testing.T) {
	if got := (&Config{}).IATAWarnInterval(); got != 6*time.Hour {
		t.Errorf("default interval %v, want 6h", got)
	}
	if got := (&Config{IATAWarnIntervalSec: -5}).IATAWarnInterval(); got != 6*time.Hour {
		t.Errorf("negative interval %v, want the default", got)
	}
	if got := (&Config{IATAWarnIntervalSec: 90}).IATAWarnInterval(); got != 90*time.Second {
		t.Errorf("configured interval %v, want 90s", got)
	}
	var nilCfg *Config
	if got := nilCfg.IATAWarnInterval(); got != 6*time.Hour {
		t.Errorf("nil config interval %v", got)
	}
}

// A huge configured interval must not overflow time.Duration (int64
// nanoseconds) into a negative interval, which would log every drop: the
// flood the throttle exists to prevent (#150). It is capped at 24 hours.
func TestIATAWarnIntervalClampsHugeValues(t *testing.T) {
	var huge int64 = 1e10 // seconds; 1e19 ns overflows int64
	for _, sec := range []int{int(huge), math.MaxInt, 24*60*60 + 1, 365 * 24 * 60 * 60} {
		if got := (&Config{IATAWarnIntervalSec: sec}).IATAWarnInterval(); got != 24*time.Hour {
			t.Errorf("iataWarnIntervalSec=%d: interval %v, want the 24h cap", sec, got)
		}
	}
	if got := (&Config{IATAWarnIntervalSec: 24 * 60 * 60}).IATAWarnInterval(); got != 24*time.Hour {
		t.Errorf("exactly 24h: interval %v", got)
	}
	if got := (&Config{IATAWarnIntervalSec: 24*60*60 - 1}).IATAWarnInterval(); got != 24*time.Hour-time.Second {
		t.Errorf("just under the cap: interval %v", got)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"observerIATAWhitelist":["ARN"],"iataWarnIntervalSec":10000000000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.IATAWarnInterval(); got != 24*time.Hour {
		t.Fatalf("iataWarnIntervalSec=10000000000 from JSON: interval %v, want 24h", got)
	}
	// and the throttle built on it still suppresses a repeat
	if w, _ := cfg.iataDropWarn.shouldWarn("GOT", t0IATA, cfg.IATAWarnInterval()); !w {
		t.Fatal("first drop not logged")
	}
	if w, _ := cfg.iataDropWarn.shouldWarn("GOT", t0IATA.Add(time.Minute), cfg.IATAWarnInterval()); w {
		t.Error("a repeat a minute later was logged again")
	}
}

func TestIATAWarnIntervalFromJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"observerIATAWhitelist":["ARN"],"iataWarnIntervalSec":600}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IATAWarnInterval() != 10*time.Minute {
		t.Errorf("iataWarnIntervalSec not read: %v", cfg.IATAWarnInterval())
	}
}

func TestIATADropThrottlePerRegionAndExpiry(t *testing.T) {
	var th iataDropThrottle
	iv := time.Hour
	if w, o := th.shouldWarn("GOT", t0IATA, iv); !w || o {
		t.Fatalf("first drop: warn=%v overflow=%v", w, o)
	}
	if w, _ := th.shouldWarn("GOT", t0IATA.Add(59*time.Minute), iv); w {
		t.Error("repeat within the interval must be suppressed")
	}
	if w, _ := th.shouldWarn("MMX", t0IATA.Add(time.Minute), iv); !w {
		t.Error("another region has its own throttle")
	}
	if w, _ := th.shouldWarn("GOT", t0IATA.Add(time.Hour), iv); !w {
		t.Error("re-log once the interval has passed")
	}
	if w, _ := th.shouldWarn("GOT", t0IATA.Add(time.Hour+time.Second), iv); w {
		t.Error("the re-log restarts the interval")
	}
}

func TestNormalizeIATAForWarn(t *testing.T) {
	for in, want := range map[string]string{
		" got ":                  "GOT",
		"cph":                    "CPH",
		strings.Repeat("a", 100): strings.Repeat("A", iataWarnMaxKeyLen),
	} {
		if got := normalizeIATAForWarn(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
	// a multi-byte rune straddling the cut is dropped, not split
	in := strings.Repeat("A", iataWarnMaxKeyLen-1) + "Ø"
	got := normalizeIATAForWarn(in)
	if len(got) > iataWarnMaxKeyLen || !strings.HasPrefix(got, strings.Repeat("A", iataWarnMaxKeyLen-1)) || strings.ContainsRune(got, '�') {
		t.Errorf("normalize(%q) = %q", in, got)
	}
}

// More distinct publisher-chosen codes than the cap: the table never grows
// past it, drops beyond it warn through one shared throttle, and that shared
// warning is re-armed after the interval.
func TestIATADropThrottleIsBoundedWithSharedOverflow(t *testing.T) {
	var th iataDropThrottle
	iv := time.Hour
	now := t0IATA
	own, overflowWarns, silent := 0, 0, 0
	for i := 0; i < 20000; i++ {
		w, o := th.shouldWarn(fmt.Sprintf("Z%05d", i), now, iv)
		switch {
		case w && !o:
			own++
		case w && o:
			overflowWarns++
		default:
			silent++
		}
		if len(th.last) > iataWarnMaxTracked {
			t.Fatalf("throttle table grew to %d entries", len(th.last))
		}
	}
	if own != iataWarnMaxTracked || overflowWarns != 1 || silent != 20000-iataWarnMaxTracked-1 {
		t.Fatalf("own=%d overflow=%d silent=%d", own, overflowWarns, silent)
	}
	// tracked codes keep their own throttle while the table is full
	if w, o := th.shouldWarn("Z00000", now.Add(iv), iv); !w || o {
		t.Errorf("a tracked code re-logs on its own slot: warn=%v overflow=%v", w, o)
	}
}

// Once the entries are older than the interval, a new region reclaims a
// slot instead of the first codes ever seen owning the table forever.
func TestIATADropThrottleReclaimsExpiredSlots(t *testing.T) {
	var th iataDropThrottle
	iv := time.Hour
	for i := 0; i < iataWarnMaxTracked; i++ {
		th.shouldWarn(fmt.Sprintf("OLD%04d", i), t0IATA, iv)
	}
	if w, o := th.shouldWarn("EARLY", t0IATA.Add(time.Minute), iv); !w || !o {
		t.Fatalf("full table of fresh entries: warn=%v overflow=%v, want the overflow warning", w, o)
	}
	later := t0IATA.Add(iv)
	if w, o := th.shouldWarn("NEW", later, iv); !w || o {
		t.Fatalf("expired table: warn=%v overflow=%v, want an own slot", w, o)
	}
	if _, ok := th.last["NEW"]; !ok || len(th.last) != 1 {
		t.Fatalf("after reclaiming: %d entries, NEW tracked=%v", len(th.last), ok)
	}
	if w, _ := th.shouldWarn("NEW", later.Add(time.Minute), iv); w {
		t.Error("the reclaimed slot throttles NEW on its own")
	}
}

// A hostile feed at the cap must not trigger a full-table scan per message:
// sweeps run only when an entry can actually have expired.
func TestIATADropThrottleSweepsAreAmortized(t *testing.T) {
	var th iataDropThrottle
	iv := time.Hour
	now := t0IATA
	for i := 0; i < 100000; i++ {
		th.shouldWarn(fmt.Sprintf("H%06d", i), now.Add(time.Duration(i)*time.Millisecond), iv)
	}
	// 100000 drops over 100s, all within one interval: at most one sweep
	// (when the table first fills; nothing can have expired after that).
	if th.sweeps > 1 {
		t.Fatalf("%d full-table sweeps for 100000 drops within one interval", th.sweeps)
	}
	// after the interval the next new code sweeps once and reclaims
	th.shouldWarn("LATE", now.Add(2*iv), iv)
	if th.sweeps > 2 || len(th.last) != 1 {
		t.Fatalf("sweeps=%d entries=%d after expiry", th.sweeps, len(th.last))
	}
}

// fillIATA tracks n new codes with the given prefix at ts; each must get its
// own slot.
func fillIATA(t *testing.T, th *iataDropThrottle, prefix string, n int, ts time.Time, iv time.Duration) {
	t.Helper()
	for i := 0; i < n; i++ {
		if w, o := th.shouldWarn(fmt.Sprintf("%s%04d", prefix, i), ts, iv); !w || o {
			t.Fatalf("%s%04d at %v: warn=%v overflow=%v, want an own slot", prefix, i, ts.Sub(t0IATA), w, o)
		}
	}
}

// oldest is a lower bound on the oldest tracked time (#150). With entries
// of different ages it must follow the oldest one still tracked: too high
// and an expired slot is not reclaimed (a new region falls into the shared
// overflow throttle), too low and a full table of fresh entries is swept
// on every drop.
func TestIATADropThrottleOldestTracksStaggeredEntries(t *testing.T) {
	var th iataDropThrottle
	iv := time.Hour
	half := iataWarnMaxTracked / 2
	fillIATA(t, &th, "A", half, t0IATA, iv)
	fillIATA(t, &th, "B", iataWarnMaxTracked-half, t0IATA.Add(30*time.Minute), iv)

	// full of fresh entries: overflow, and no sweep (nothing can be expired)
	if w, o := th.shouldWarn("EARLY", t0IATA.Add(40*time.Minute), iv); !w || !o {
		t.Fatalf("full table at +40m: warn=%v overflow=%v, want the overflow warning", w, o)
	}
	if th.sweeps != 0 {
		t.Fatalf("%d sweeps before any entry could expire", th.sweeps)
	}

	// +1h: the A half has expired; a new region reclaims a slot
	if w, o := th.shouldWarn("N1", t0IATA.Add(time.Hour), iv); !w || o {
		t.Fatalf("+1h: warn=%v overflow=%v, want an own slot (A entries expired)", w, o)
	}
	if len(th.last) != iataWarnMaxTracked-half+1 || th.sweeps != 1 {
		t.Fatalf("+1h: %d entries after %d sweeps, want %d after 1", len(th.last), th.sweeps, iataWarnMaxTracked-half+1)
	}
	// refill to the cap with entries at +1h
	fillIATA(t, &th, "C", half-1, t0IATA.Add(time.Hour), iv)

	// +1h30m: the B half (+30m) has expired now and must be reclaimed,
	// although every entry swept at +1h is gone and the newest are fresh
	if w, o := th.shouldWarn("N2", t0IATA.Add(90*time.Minute), iv); !w || o {
		t.Fatalf("+1h30m: warn=%v overflow=%v, want an own slot (B entries expired)", w, o)
	}
	if _, ok := th.last["B0000"]; ok {
		t.Error("expired B entries were not reclaimed at +1h30m")
	}
	if len(th.last) != half+1 || th.sweeps != 2 {
		t.Fatalf("+1h30m: %d entries after %d sweeps, want %d after 2", len(th.last), th.sweeps, half+1)
	}
}

// A drop can carry an earlier time than the current oldest entry (the
// caller takes the time before the lock). oldest must drop to it, or that
// entry outlives its interval in a full table.
func TestIATADropThrottleOldestFollowsAnEarlierDrop(t *testing.T) {
	var th iataDropThrottle
	iv := time.Hour
	fillIATA(t, &th, "A", iataWarnMaxTracked, t0IATA, iv)
	// +1h: everything expired; sweep, keep only LATE (taken at +1h)
	if w, o := th.shouldWarn("LATE", t0IATA.Add(time.Hour), iv); !w || o {
		t.Fatalf("+1h: warn=%v overflow=%v", w, o)
	}
	// a drop stamped +50m arrives after it
	if w, o := th.shouldWarn("EARLIER", t0IATA.Add(50*time.Minute), iv); !w || o {
		t.Fatalf("out-of-order drop: warn=%v overflow=%v", w, o)
	}
	fillIATA(t, &th, "F", iataWarnMaxTracked-2, t0IATA.Add(time.Hour), iv)
	// +1h50m: EARLIER has expired and must free its slot
	if w, o := th.shouldWarn("NEW", t0IATA.Add(110*time.Minute), iv); !w || o {
		t.Fatalf("+1h50m: warn=%v overflow=%v, want EARLIER's expired slot", w, o)
	}
	if _, ok := th.last["EARLIER"]; ok {
		t.Error("EARLIER outlived its interval in a full table")
	}
}

func TestIATADropThrottleConcurrent(t *testing.T) {
	var th iataDropThrottle
	iv := time.Hour
	var wg sync.WaitGroup
	var mu sync.Mutex
	warns := 0
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				code := "SAME"
				if i%2 == 1 {
					code = fmt.Sprintf("G%02dI%03d", g, i)
				}
				if w, _ := th.shouldWarn(code, t0IATA, iv); w && code == "SAME" {
					mu.Lock()
					warns++
					mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()
	if warns != 1 {
		t.Fatalf("SAME warned %d times from 16 goroutines within one interval", warns)
	}
	if len(th.last) > iataWarnMaxTracked {
		t.Fatalf("%d entries", len(th.last))
	}
}

func BenchmarkIATADropThrottleHostileAtCap(b *testing.B) {
	var th iataDropThrottle
	iv := time.Hour
	codes := make([]string, 4096)
	for i := range codes {
		codes[i] = fmt.Sprintf("B%05d", i)
	}
	for i := 0; i < iataWarnMaxTracked; i++ {
		th.shouldWarn(codes[i], t0IATA, iv)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		th.shouldWarn(codes[i%len(codes)], t0IATA.Add(time.Duration(i)), iv)
	}
}
