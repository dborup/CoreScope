package channel

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const expectedRainbowEntryCount = 320

func TestChannelRainbowMatchesDeriveKey(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "channel-rainbow.json"))
	if err != nil {
		t.Fatalf("read channel-rainbow.json: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("channel-rainbow.json is not valid JSON")
	}

	entries := decodeUniqueRainbowEntries(t, data)
	if got := len(entries); got != expectedRainbowEntryCount {
		t.Fatalf("channel-rainbow.json has %d entries, want %d", got, expectedRainbowEntryCount)
	}

	seenKeys := make(map[string]string, len(entries))
	for name, got := range entries {
		if otherName, exists := seenKeys[got]; exists {
			t.Errorf("duplicate channel key %q used by %q and %q", got, otherName, name)
		} else {
			seenKeys[got] = name
		}

		if name == "Public" {
			// Public is the firmware-default PSK, not a hashtag-derived key.
			if want := "8b3387e9c5cdea6ac9e5edbaa115cd72"; got != want {
				t.Errorf("Public key = %q, want firmware default %q", got, want)
			}
			continue
		}

		want := hex.EncodeToString(DeriveKey(name))
		if got != want {
			t.Errorf("%s key = %q, want DeriveKey result %q", name, got, want)
		}
	}
}

func decodeUniqueRainbowEntries(t *testing.T, data []byte) map[string]string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))

	opening, err := dec.Token()
	if err != nil {
		t.Fatalf("decode opening token: %v", err)
	}
	if opening != json.Delim('{') {
		t.Fatalf("channel-rainbow.json must contain an object, got %v", opening)
	}

	entries := make(map[string]string)
	for dec.More() {
		nameToken, err := dec.Token()
		if err != nil {
			t.Fatalf("decode channel name: %v", err)
		}
		name, ok := nameToken.(string)
		if !ok {
			t.Fatalf("channel name must be a string, got %T", nameToken)
		}
		if _, exists := entries[name]; exists {
			t.Fatalf("duplicate channel name %q", name)
		}

		var key string
		if err := dec.Decode(&key); err != nil {
			t.Fatalf("decode key for %q: %v", name, err)
		}
		entries[name] = key
	}

	closing, err := dec.Token()
	if err != nil {
		t.Fatalf("decode closing token: %v", err)
	}
	if closing != json.Delim('}') {
		t.Fatalf("channel-rainbow.json object is not closed, got %v", closing)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("unexpected content after channel-rainbow.json object: %v", err)
	}

	return entries
}
