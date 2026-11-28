package main

import (
	"log"
	"strings"
	"testing"
)

// TestSchemaDegradationLogIsPerStore asserts that two independent
// PacketStore instances both emit a schema-degradation warning for the
// same message. With the (pre-#1199) package-level sync.Map, the second
// instance silently swallows the warning — that is order-dependent test
// pollution and is exactly what item 5/6 of #1199 calls out.
//
// RED: today, only the first store logs; the second is suppressed by the
// package-level sentinel. GREEN follow-up moves the sentinel to a
// PacketStore field so each instance has a fresh dedupe set.
func TestSchemaDegradationLogIsPerStore(t *testing.T) {
	const msg = "test-schema-degradation-marker-1199"

	// The capture is locked (#310): goroutines from earlier tests keep
	// logging into whatever writer is installed.
	out := captureLog(func() {
		log.SetFlags(0)
		s1 := &PacketStore{}
		s2 := &PacketStore{}
		s1.logSchemaDegradationOnce(msg)
		s2.logSchemaDegradationOnce(msg)
	})

	hits := strings.Count(out, msg)
	if hits != 2 {
		t.Fatalf("expected 2 log emissions (one per PacketStore), got %d. "+
			"package-level sentinel pollutes across instances — move to a "+
			"struct field. log buffer:\n%s", hits, out)
	}
}
