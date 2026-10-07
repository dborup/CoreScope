package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Packet.h defines PH_TYPE_MASK=0x0F: include reserved wire codes, not just
// the currently named types. A mask bounds storage and canonicalizes duplicates.
type packetTypeExclusions uint16

func parsePacketTypeExclusions(values url.Values) (packetTypeExclusions, error) {
	entries := values["excludeTypes"]
	if len(entries) == 0 {
		return 0, nil
	}
	if len(entries) != 1 || len(entries[0]) > 64 {
		return 0, fmt.Errorf("excludeTypes must be one comma-separated list of at most 16 types (0-15)")
	}
	if entries[0] == "" {
		return 0, nil
	}
	parts := strings.Split(entries[0], ",")
	if len(parts) > 16 {
		return 0, fmt.Errorf("excludeTypes accepts at most 16 entries")
	}
	var mask packetTypeExclusions
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) < 1 || len(part) > 2 || part[0] < '0' || part[0] > '9' {
			return 0, fmt.Errorf("excludeTypes entries must be integers from 0 to 15")
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 15 {
			return 0, fmt.Errorf("excludeTypes entries must be integers from 0 to 15")
		}
		mask |= 1 << uint(n)
	}
	return mask, nil
}

// Test mask first: the default (no exclusion) must not dereference every
// tx's separately allocated PayloadType in the filter loop.
func (mask packetTypeExclusions) excludes(typ *int) bool {
	return mask != 0 && typ != nil && *typ >= 0 && *typ < 16 && mask&(1<<uint(*typ)) != 0
}

// Keep unknown/NULL payload types, matching the in-memory predicate. The
// caller supplies a fixed SQL column name; all values remain parameters.
func (mask packetTypeExclusions) appendSQL(where []string, args []interface{}, column string) ([]string, []interface{}) {
	if mask == 0 {
		return where, args
	}
	placeholders := make([]string, 0, 16)
	for typ := 0; typ < 16; typ++ {
		if mask&(1<<uint(typ)) != 0 {
			placeholders = append(placeholders, "?")
			args = append(args, typ)
		}
	}
	where = append(where, "("+column+" IS NULL OR "+column+" NOT IN ("+strings.Join(placeholders, ",")+"))")
	return where, args
}
