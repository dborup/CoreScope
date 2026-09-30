package main

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Every code in the region parameter becomes part of the key, so a long
// parameter must not be kept as a map key (and flight key) byte for byte.
// Long keys are replaced by a fixed-size digest; they are still cached,
// coalesced and kept apart per region, and the entry bound still holds.
func TestChannelListCacheKeyLengthIsBounded_109(t *testing.T) {
	db := singleflightDB(t)
	q := &queryCounter{want: 1}
	db.channelsQueryHook = q.hook
	many := make([]string, 300)
	for i := range many {
		many[i] = "R" + strconv.Itoa(i)
	}
	regions := []string{
		strings.Join(many, ","),          // many short codes
		strings.Repeat("Y", 1<<20),       // one 1 MiB code
		strings.Repeat("Y", 1<<20) + "Z", // differs only in the last byte
		"AAR",
	}
	for _, r := range regions {
		for i := 0; i < 2; i++ { // the second call is served from the cache
			if _, err := db.GetChannels(r); err != nil {
				t.Fatal(err)
			}
			if _, err := db.GetEncryptedChannels(r); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, kind := range []string{"channels", "encrypted"} {
		if got := q.count(kind); got != len(regions) {
			t.Errorf("%s: %d queries for %d regions called twice each, want one per region", kind, got, len(regions))
		}
	}
	for name, c := range map[string]*channelListCache{"channels": &db.channelsCache, "encrypted": &db.encChannelsCache} {
		if len(c.entries) != len(regions) {
			t.Errorf("%s: %d entries for %d regions", name, len(c.entries), len(regions))
		}
		for k := range c.entries {
			if len(k) > channelListMaxKeyBytes {
				t.Errorf("%s: cache key of %d bytes kept, limit %d", name, len(k), channelListMaxKeyBytes)
			}
		}
		if _, ok := c.entries["AAR"]; !ok {
			t.Errorf("%s: a short key is not stored as itself", name)
		}
	}
}

// Only the cache and the flight use the bounded key: the query itself must
// still get the full normalized region, or a long region parameter would be
// looked up as the region "sha256:…" and cache an empty list.
func TestChannelListLongKeyQueriesFullRegion_109(t *testing.T) {
	db := singleflightDB(t)
	var mu sync.Mutex
	got := map[string][]string{}
	db.channelsQueryHook = func(kind, region string) error {
		mu.Lock()
		got[kind] = append(got[kind], region)
		mu.Unlock()
		return nil
	}
	many := make([]string, 300)
	for i := range many {
		many[i] = "r" + strconv.Itoa(i)
	}
	param := strings.Join(many, ",")
	want := channelsRegionKey(param)
	if len(want) <= channelListMaxKeyBytes {
		t.Fatalf("test region key is %d bytes, want more than %d", len(want), channelListMaxKeyBytes)
	}
	if _, err := db.GetChannels(param); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetEncryptedChannels(param); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"channels", "encrypted"} {
		if len(got[kind]) != 1 {
			t.Fatalf("%s: %d queries, want 1", kind, len(got[kind]))
		}
		if got[kind][0] != want {
			t.Errorf("%s: query got region %.40q…, want the full normalized key %.40q…", kind, got[kind][0], want)
		}
	}
}
