//go:build race

package dhm

// raceEnabled skips the timing tests under -race, which slows them ~10x.
const raceEnabled = true
