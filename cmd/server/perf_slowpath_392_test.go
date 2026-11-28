package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// #392: normalizeSlowPath pairs the :id-folded route key with a sanitised,
// length-capped raw path. It is read-only and does NOT HTML-escape (the perf
// dashboard does that at render time) — these tests pin length-capping,
// control-char stripping and rune-safety, which a mutant dropping the cap or
// the strip must break.
func TestNormalizeSlowPath(t *testing.T) {
	t.Run("key and raw differ: emits template plus raw", func(t *testing.T) {
		got := normalizeSlowPath("/api/nodes/:id", "/api/nodes/abc123")
		if got != "/api/nodes/:id /api/nodes/abc123" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("unmatched route (key == raw): raw only, no duplication", func(t *testing.T) {
		got := normalizeSlowPath("/api/nope", "/api/nope")
		if got != "/api/nope" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("long raw is capped to slowPathRawCap runes plus ellipsis", func(t *testing.T) {
		raw := "/api/" + strings.Repeat("a", 500)
		got := normalizeSlowPath(raw, raw) // key==raw so result is just the capped raw
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("expected truncation ellipsis, got %q", got)
		}
		// Capped body is exactly slowPathRawCap runes; +1 for the ellipsis.
		if n := utf8.RuneCountInString(got); n != slowPathRawCap+1 {
			t.Fatalf("expected %d runes, got %d (%q)", slowPathRawCap+1, n, got[:40])
		}
	})

	t.Run("short raw is left intact (no ellipsis)", func(t *testing.T) {
		got := normalizeSlowPath("/api/x", "/api/x")
		if strings.Contains(got, "…") {
			t.Fatalf("short path should not be truncated: %q", got)
		}
	})

	t.Run("control characters are stripped", func(t *testing.T) {
		got := normalizeSlowPath("/api/x", "/api/x\r\n\tevil")
		if strings.ContainsAny(got, "\r\n\t") {
			t.Fatalf("control chars survived: %q", got)
		}
		if got != "/api/x/api/xevil" && got != "/api/x /api/xevil" {
			// key=="/api/x", capped raw=="/api/xevil" → "/api/x /api/xevil".
			t.Fatalf("unexpected result: %q", got)
		}
	})

	t.Run("multibyte rune is not split at the cap boundary", func(t *testing.T) {
		// 200 × 'é' (2 bytes each). A byte-slice cap would split the rune and
		// yield invalid UTF-8; the rune-safe cap must not.
		raw := strings.Repeat("é", 200)
		got := normalizeSlowPath(raw, raw)
		if !utf8.ValidString(got) {
			t.Fatalf("cap split a multibyte rune (invalid UTF-8): %q", got[:20])
		}
	})

	t.Run("does NOT HTML-escape (escaping is the frontend's job)", func(t *testing.T) {
		raw := "/api/<img src=x>"
		got := normalizeSlowPath("/api/<img src=x>", raw)
		if !strings.Contains(got, "<img src=x>") {
			t.Fatalf("server must not escape; perf.js does: %q", got)
		}
	})

	// #394 F3: the cap must bound the WHOLE stored value, not just the raw
	// half. A pathological route key (e.g. a template that itself folded in a
	// long unmatched segment) must not slip past the cap just because it is
	// on the key side of the join. Mutant: cap only the raw half → red.
	t.Run("long template key is also capped (total value is bounded)", func(t *testing.T) {
		key := "/api/:id/" + strings.Repeat("z", 5000)
		raw := "/api/deadbeef12/" + strings.Repeat("z", 5000)
		got := normalizeSlowPath(key, raw)
		// Each half caps at slowPathRawCap runes (+ an ellipsis); the two are
		// joined by one space. The total can therefore never exceed
		// 2*cap + 2 ellipses + 1 space.
		max := 2*slowPathRawCap + 3
		if n := utf8.RuneCountInString(got); n > max {
			t.Fatalf("stored value not bounded: %d runes (max %d)", n, max)
		}
		if !strings.Contains(got, "…") {
			t.Fatalf("expected truncation ellipsis in the oversized value: %q", got[:40])
		}
	})

	// #394 F4: control characters must be stripped from BOTH halves. A CR/LF
	// in the normalized route key would otherwise survive into the stored
	// value (log/row injection). Mutant: strip only the raw half → red.
	t.Run("control characters are stripped from the key half too", func(t *testing.T) {
		key := "/api/\r\nX-Injected: 1/:id"
		raw := "/api/deadbeef12"
		got := normalizeSlowPath(key, raw)
		if strings.ContainsAny(got, "\r\n\t") {
			t.Fatalf("control chars survived in the key half: %q", got)
		}
	})

	// #393 N4: the strip covers DEL (0x7f) as well as C0 (< 0x20); only the
	// C0 half was pinned. Neighbouring '~' (0x7e) must survive. Mutant: drop
	// `|| r == 0x7f` → red.
	t.Run("DEL is stripped from both halves, '~' is kept", func(t *testing.T) {
		if got := sanitizeSlowPathComponent("a\x7fb~"); got != "ab~" {
			t.Fatalf("sanitizeSlowPathComponent: got %q, want %q", got, "ab~")
		}
		got := normalizeSlowPath("/api/\x7f:id", "/api/x\x7fy")
		if got != "/api/:id /api/xy" {
			t.Fatalf("normalizeSlowPath: got %q, want %q", got, "/api/:id /api/xy")
		}
	})
}
