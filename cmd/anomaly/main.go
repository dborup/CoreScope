// corescope-anomaly is an EXPERIMENTAL, OFFLINE tool for the
// internal/anomaly study (issue #457). It is not part of the server or the
// ingestor and is not built into any image.
//
//	corescope-anomaly export -db PATH [-immutable] -start T -end T -settle D -max-rows N -out FILE
//	    [-backlog-silence D -backlog-span D | -late-window D -late-min-rows N -late-share F -late-gap D]
//	corescope-anomaly replay -config FILE -input FILE|- -start T -end T -max-line-bytes N [-out FILE]
//	corescope-anomaly keys -input FILE|- -payload-type N -top N [-exclude-channels LIST] -max-keys N -max-line-bytes N [-out FILE]
//	corescope-anomaly inject -spec FILE -input FILE|- -out FILE -labels FILE -max-injected N -max-line-bytes N
//	corescope-anomaly score -report FILE [-labels FILE] [-baseline FILE] [-real-labels FILE] -rule NAME -start T -end T [-out FILE]
//	corescope-anomaly label -input FILE|- -payload-type N [-exclude-channels LIST] -start T -end T
//	    -pulse-merge-gap D -min-period D -max-period D -min-pulses N -max-harmonic N
//	    -tolerance-abs D -tolerance-rel F -min-size-share F -max-lines N -max-line-bytes N [-out FILE]
//
// export reads a CoreScope SQLite database read-only (mode=ro; -immutable for
// a static snapshot copy) and writes anomaly.ReplayRecord JSON Lines built
// from on-wire fields only, with per-export IDs. replay runs anomaly.Replay
// over such lines and prints the deterministic report. keys ranks the stream
// keys of such lines by row count, so an injection panel can be placed on
// keys real traffic uses. inject overlays synthetic channel-bot trains
// (clearly marked "inj-..." ids) on such lines and writes the ground truth
// beside them; score measures a replay report against that ground truth
// (#474). label writes ground truth for real traffic from aggregate
// signals alone - a channel-hash periodic run whose lines share one payload
// size - so score can tell a real automated sender from a false alarm in
// the false-alarm arm (#474 round 4). Only export reads a database, and
// nothing here ever writes to one. Every bound is an explicit flag; there
// are no defaults.
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
      [-backlog-silence DURATION -backlog-span DURATION | -late-window DURATION -late-min-rows N -late-share F -late-gap DURATION]
  corescope-anomaly replay -config FILE -input FILE|- -start RFC3339 -end RFC3339 -max-line-bytes N [-out FILE]
  corescope-anomaly keys -input FILE|- -payload-type N -top N [-exclude-channels LIST] -max-keys N -max-line-bytes N [-out FILE]
  corescope-anomaly inject -spec FILE -input FILE|- -out FILE -labels FILE -max-injected N -max-line-bytes N
  corescope-anomaly score -report FILE [-labels FILE] [-baseline FILE] [-real-labels FILE] -rule NAME -start RFC3339 -end RFC3339 [-out FILE]
  corescope-anomaly label -input FILE|- -payload-type N [-exclude-channels LIST] -start RFC3339 -end RFC3339
      -pulse-merge-gap DURATION -min-period DURATION -max-period DURATION -min-pulses N -max-harmonic N
      -tolerance-abs DURATION -tolerance-rel F -min-size-share F -max-lines N -max-line-bytes N [-out FILE]
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
	case "keys":
		return runKeys(args[1:], stdin, stdout, stderr)
	case "inject":
		return runInject(args[1:], stdin, stderr)
	case "score":
		return runScore(args[1:], stdout, stderr)
	case "label":
		return runLabel(args[1:], stdin, stdout, stderr)
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
