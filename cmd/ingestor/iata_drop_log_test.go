package main

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
)

// #110: when observerIATAWhitelist rejects a region, the drop used to be
// silent, so a legitimate region missing from the list lost all its traffic
// with nothing in the log. These tests drive the real handleMessage and read
// the log it writes.

// captureLog runs fn with the standard logger writing to a buffer.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(orig); log.SetFlags(flags) }()
	fn()
	return buf.String()
}

func regionFilterLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[region-filter]") {
			lines = append(lines, l)
		}
	}
	return lines
}

func statusMsg(region, observer string) *mockMessage {
	return &mockMessage{
		topic:   "meshcore/" + region + "/" + observer + "/status",
		payload: []byte(`{"origin":"n","noise_floor":-110}`),
	}
}

func TestIATAWhitelistDropIsLoggedOncePerRegion(t *testing.T) {
	store := newTestStore(t)
	cfg := &Config{ObserverIATAWhitelist: []string{"ARN"}}
	out := captureLog(t, func() {
		for i := 0; i < 5; i++ {
			handleMessage(store, "src", MQTTSource{Name: "src"}, statusMsg("got", fmt.Sprintf("obs%d", i)), nil, nil, cfg)
		}
	})
	lines := regionFilterLines(out)
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 [region-filter] line for 5 drops of one region, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], `"GOT"`) || !strings.Contains(lines[0], "observerIATAWhitelist") {
		t.Errorf("warning must name the normalized region and the setting: %q", lines[0])
	}
	var n int
	store.db.QueryRow("SELECT COUNT(*) FROM observers").Scan(&n)
	if n != 0 {
		t.Errorf("dropped messages must still be dropped, %d observers stored", n)
	}
}

func TestIATAWhitelistDropWarnsPerDistinctRegion(t *testing.T) {
	store := newTestStore(t)
	cfg := &Config{ObserverIATAWhitelist: []string{"ARN"}}
	out := captureLog(t, func() {
		for _, r := range []string{"GOT", "MMX", " got ", "CPH", "mmx"} {
			handleMessage(store, "src", MQTTSource{Name: "src"}, statusMsg(r, "o"), nil, nil, cfg)
		}
	})
	lines := regionFilterLines(out)
	if len(lines) != 3 {
		t.Fatalf("want one line each for GOT, MMX, CPH (codes are normalized), got %d:\n%s", len(lines), out)
	}
}

func TestIATAWhitelistAllowedAndEmptyAreSilent(t *testing.T) {
	store := newTestStore(t)
	out := captureLog(t, func() {
		handleMessage(store, "src", MQTTSource{Name: "src"}, statusMsg("ARN", "a1"), nil, nil, &Config{ObserverIATAWhitelist: []string{"arn"}})
		handleMessage(store, "src", MQTTSource{Name: "src"}, statusMsg("GOT", "a2"), nil, nil, &Config{})
	})
	if lines := regionFilterLines(out); len(lines) != 0 {
		t.Fatalf("allowed traffic and an empty whitelist must not warn:\n%s", out)
	}
	var n int
	store.db.QueryRow("SELECT COUNT(*) FROM observers").Scan(&n)
	if n != 2 {
		t.Errorf("allowed traffic must still be stored, got %d observers", n)
	}
}

// The region is a topic segment the publisher controls: it must not be able
// to forge log lines or make one line arbitrarily long.
func TestIATAWhitelistDropWarningIsOneEscapedBoundedLine(t *testing.T) {
	store := newTestStore(t)
	cfg := &Config{ObserverIATAWhitelist: []string{"ARN"}}
	evil := "XX\nMQTT [src] fake line\r" + strings.Repeat("A", 10000)
	out := captureLog(t, func() {
		handleMessage(store, "src", MQTTSource{Name: "src"}, statusMsg(evil, "o"), nil, nil, cfg)
	})
	lines := regionFilterLines(out)
	if len(lines) != 1 {
		t.Fatalf("want 1 warning line, got %d:\n%.500s", len(lines), out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("the warning spans %d lines; control characters must be escaped", strings.Count(out, "\n"))
	}
	if len(lines[0]) > 300 {
		t.Errorf("warning line is %d bytes; the region must be truncated", len(lines[0]))
	}
}

// Many distinct attacker-chosen regions: the warning keeps coming (never
// silent), but through a shared overflow path, so the number of lines stays
// bounded well below the number of regions.
func TestIATAWhitelistDropManyDistinctRegionsStaysBoundedAndVisible(t *testing.T) {
	store := newTestStore(t)
	cfg := &Config{ObserverIATAWhitelist: []string{"ARN"}}
	const distinct = 3000
	out := captureLog(t, func() {
		for i := 0; i < distinct; i++ {
			handleMessage(store, "src", MQTTSource{Name: "src"}, statusMsg(fmt.Sprintf("Z%05d", i), "o"), nil, nil, cfg)
		}
	})
	lines := regionFilterLines(out)
	if len(lines) == 0 {
		t.Fatal("drops beyond the throttle bound must not be silent")
	}
	if len(lines) >= distinct/2 {
		t.Fatalf("%d warning lines for %d regions: the per-region throttle must be bounded", len(lines), distinct)
	}
	overflow := 0
	for _, l := range lines {
		if strings.Contains(l, "throttle table full") {
			overflow++
		}
	}
	if overflow != 1 {
		t.Errorf("want exactly one shared overflow warning within the interval, got %d", overflow)
	}
}
