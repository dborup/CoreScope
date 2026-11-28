package main

// The bounded optional numbers of a config block, shared by the terrain
// profile (map.dhm.profile, #681) and the heritage query (map.heritage,
// #723 M2).

// configKnobs reads optional numeric values of one config block. A nil
// value takes the default; an out-of-range one logs a warning that names
// the key (never the value) and takes the default too.
type configKnobs struct {
	tag   string // log prefix, e.g. "[dhm]"
	block string // config path, e.g. "map.dhm.profile"
	logf  func(string, ...interface{})
}

// num accepts lo..hi, excluding lo when loOpen. NaN is out of range.
func (k configKnobs) num(key string, v *float64, def, lo, hi float64, loOpen bool) float64 {
	if v == nil {
		return def
	}
	ok := *v <= hi && (*v > lo || !loOpen && *v == lo) // NaN fails both
	if !ok {
		k.logf("%s config %s.%s is out of range; using %v", k.tag, k.block, key, def)
		return def
	}
	return *v
}

// integer accepts lo..hi inclusive.
func (k configKnobs) integer(key string, v *int, def, lo, hi int) int {
	if v == nil {
		return def
	}
	if *v < lo || *v > hi {
		k.logf("%s config %s.%s is out of range; using %d", k.tag, k.block, key, def)
		return def
	}
	return *v
}
