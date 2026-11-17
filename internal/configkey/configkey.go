// Package configkey reads config.json members by their canonical, exact-case
// keys (#884). encoding/json matches object keys case-insensitively, so a
// struct field tagged `json:"enabled"` also takes `Enabled` or `ENABLED`,
// and whichever comes last wins, even after a canonical `"enabled": false`.
// A feature gate must not depend on that: the server and the ingestor read
// their gates through this package, and both read them the same way.
package configkey

import (
	"encoding/json"
	"sort"
	"strings"
)

// Doc is a JSON document read by exact keys. Every key that matches a
// requested key only case-insensitively is ignored and reported once.
type Doc struct {
	raw    []byte
	warn   func(format string, args ...interface{})
	warned map[string]bool
}

// New returns a Doc over raw. warn (log.Printf or similar) receives one
// line per ignored key; it may be nil.
func New(raw []byte, warn func(format string, args ...interface{})) *Doc {
	return &Doc{raw: raw, warn: warn, warned: make(map[string]bool)}
}

// Get returns the value at path, every key matched exactly, or nil when a
// key is missing or a value on the way is not an object. A key on the way
// that differs from the requested one only in case is ignored with a
// warning naming its path, never its value.
func (d *Doc) Get(path ...string) json.RawMessage {
	v := json.RawMessage(d.raw)
	for i, want := range path {
		var obj map[string]json.RawMessage
		if json.Unmarshal(v, &obj) != nil {
			return nil
		}
		d.warnVariants(obj, path[:i], want)
		var ok bool
		if v, ok = obj[want]; !ok {
			return nil
		}
	}
	return v
}

// On reports whether the value at path is the JSON boolean true.
func (d *Doc) On(path ...string) bool {
	var on bool
	return json.Unmarshal(d.Get(path...), &on) == nil && on
}

// warnVariants reports each key of obj that matches want only
// case-insensitively, in a stable order, once per Doc.
func (d *Doc) warnVariants(obj map[string]json.RawMessage, parent []string, want string) {
	var variants []string
	for k := range obj {
		if k != want && strings.EqualFold(k, want) {
			variants = append(variants, k)
		}
	}
	sort.Strings(variants)
	prefix := strings.Join(parent, ".")
	if prefix != "" {
		prefix += "."
	}
	for _, k := range variants {
		if d.warned[prefix+k] {
			continue
		}
		d.warned[prefix+k] = true
		if d.warn != nil {
			d.warn("[config] %s%s is ignored: config keys are case-sensitive, the key is %s%s", prefix, k, prefix, want)
		}
	}
}
