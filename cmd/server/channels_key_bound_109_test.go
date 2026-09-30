package main

import (
	"strconv"
	"strings"
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
