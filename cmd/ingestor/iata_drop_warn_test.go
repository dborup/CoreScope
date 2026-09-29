package main

import (
	"fmt"
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
