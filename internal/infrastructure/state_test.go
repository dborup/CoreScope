package infrastructure

import (
	"encoding/json"
	"testing"
)

func TestStateRoundTripAndBounds(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !ValidPublicKey(key) || ValidPublicKey("../../etc/passwd") || ValidPublicKey(key[:62]) {
		t.Fatal("public-key validation failed")
	}
	state := State{Selected: []Entry{{PublicKey: key, AddedAt: 123}}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeState(raw)
	if err != nil || len(got.Selected) != 1 || got.Selected[0] != state.Selected[0] {
		t.Fatalf("roundtrip = %+v, %v", got, err)
	}
	tooMany, _ := json.Marshal(State{Selected: append(make([]Entry, MaxSelected), Entry{PublicKey: key})})
	if _, err := decodeState(tooMany); err == nil {
		t.Fatal("accepted oversized selection")
	}
}

func TestApplyIdempotenceAndReplay(t *testing.T) {
	a := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s := State{}
	if err := s.Apply("1111111111111111", OpSelect, a, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply("2222222222222222", OpRemove, a, 20); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply("1111111111111111", OpSelect, a, 10); err != nil {
		t.Fatal(err)
	}
	if len(s.Selected) != 0 {
		t.Fatal("old selection replayed after remove")
	}
}
