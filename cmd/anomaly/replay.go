package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/meshcore-analyzer/anomaly"
)

// runReplay runs anomaly.Replay over ReplayRecord lines and prints the
// report in the format of the library's golden file.
func runReplay(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "replay config JSON (required)")
	input := fs.String("input", "", "ReplayRecord JSON Lines, or - for stdin (required)")
	start := fs.String("start", "", "RFC 3339 start, inclusive (required)")
	end := fs.String("end", "", "RFC 3339 end, exclusive (required)")
	maxLine := fs.Int("max-line-bytes", 0, "longest accepted input line (required)")
	out := fs.String("out", "", "report file (must not exist); default stdout")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	usageErr := func(err error) int {
		fmt.Fprintf(stderr, "replay: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("unexpected arguments"))
	}
	if *cfgPath == "" || *input == "" {
		return usageErr(errors.New("-config and -input are required"))
	}
	if *maxLine <= 0 {
		return usageErr(errors.New("-max-line-bytes must be > 0"))
	}
	opt := anomaly.ReplayOptions{MaxLineBytes: *maxLine}
	var err error
	if opt.Start, err = parseTime("start", *start); err != nil {
		return usageErr(err)
	}
	if opt.End, err = parseTime("end", *end); err != nil {
		return usageErr(err)
	}
	if !opt.End.After(opt.Start) {
		return usageErr(errors.New("-end must be after -start"))
	}

	fail := func(err error) int {
		fmt.Fprintf(stderr, "replay: %v\n", err)
		return exitFail
	}
	cb, err := os.ReadFile(*cfgPath)
	if err != nil {
		return fail(err)
	}
	cfg, err := anomaly.ParseReplayConfig(cb)
	if err != nil {
		return fail(err)
	}
	in := stdin
	if *input != "-" {
		f, err := os.Open(*input)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		in = f
	}
	rep, err := anomaly.Replay(in, cfg, opt)
	if err != nil {
		return fail(err)
	}
	b, err := json.MarshalIndent(rep, "", " ")
	if err != nil {
		return fail(err)
	}
	b = append(b, '\n')
	if *out == "" {
		_, err = stdout.Write(b)
	} else {
		err = writeNew(*out, b)
	}
	if err != nil {
		return fail(err)
	}
	return exitOK
}

// writeNew writes b to a file that must not exist yet.
func writeNew(path string, b []byte) error {
	f, err := createNew(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}
