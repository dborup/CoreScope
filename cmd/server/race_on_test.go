//go:build race

package main

// raceBuild reports a -race test binary, whose instrumentation makes CPU
// bound code several times slower; perf budgets scale with it.
const raceBuild = true
