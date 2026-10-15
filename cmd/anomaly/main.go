// corescope-anomaly is an EXPERIMENTAL, OFFLINE tool for the
// internal/anomaly study (issue #457). It is not part of the server or the
// ingestor and is not built into any image.
//
//	corescope-anomaly export -db PATH [-immutable] -start T -end T -settle D -max-rows N -out FILE
//	corescope-anomaly replay -config FILE -input FILE|- -start T -end T -max-line-bytes N [-out FILE]
//
// export reads a CoreScope SQLite database read-only (mode=ro; -immutable for
// a static snapshot copy) and writes anomaly.ReplayRecord JSON Lines built
// from on-wire fields only, with per-export IDs. replay runs anomaly.Replay
// over such lines and prints the deterministic report. Every bound is an
// explicit flag; there are no defaults.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const usage = `usage:
  corescope-anomaly export -db PATH [-immutable] -start RFC3339 -end RFC3339 -settle DURATION -max-rows N -out FILE
  corescope-anomaly replay -config FILE -input FILE|- -start RFC3339 -end RFC3339 -max-line-bytes N [-out FILE]
`

// Exit codes: 0 success, 1 runtime failure, 2 usage error.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "export":
		return runExport(args[1:], stderr)
	case "replay":
		return runReplay(args[1:], stdin, stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return exitUsage
}

// parseTime parses a required RFC 3339 flag value.
func parseTime(name, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, fmt.Errorf("-%s is required", name)
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("-%s: want RFC 3339", name)
	}
	return t.UTC(), nil
}

// createNew opens path for writing only if it does not exist yet.
func createNew(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%s exists; refusing to overwrite", path)
	}
	return f, err
}
