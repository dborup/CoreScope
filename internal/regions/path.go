package regions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PathEnvVar is the environment override for the hashRegionsPath config
// key, mirroring CHANNEL_KEYS_PATH / channelKeysPath. Set and non-blank, it
// wins over config.json.
const PathEnvVar = "HASH_REGIONS_PATH"

// Result is what Load made of the inline hashRegions list and the optional
// external file.
type Result struct {
	// Names is the effective set: the normalized, deduplicated union of
	// the inline entries and the file's, inline first.
	Names []string

	// Path is the resolved file Load tried to read, "" when no
	// hashRegionsPath is configured.
	Path string

	// FileNames holds the file's raw (un-normalized) entries, nil when no
	// file was read or the read failed. Callers that cache the file across
	// later config edits keep these and re-run Merge, so the union rule
	// stays in this package.
	FileNames []string

	// Duplicates lists the normalized names that appeared more than once
	// across inline+file, first-seen order. Informational.
	Duplicates []string

	// Blank counts entries that normalized to nothing.
	Blank int
}

// ConfiguredPath returns the hashRegionsPath in effect: the PathEnvVar
// value when set and non-blank, otherwise the trimmed config key. Empty
// means "no external file" — there is deliberately no auto-discovered
// default, so an unset path must never make a process read a file that
// merely happens to sit next to config.json.
func ConfiguredPath(cfgValue string) string {
	if env := strings.TrimSpace(os.Getenv(PathEnvVar)); env != "" {
		return env
	}
	return strings.TrimSpace(cfgValue)
}

// ResolvePath anchors a relative hashRegionsPath at the directory holding
// the config file it came from — the same rule channelKeysPath's default
// uses. The ingestor and the server run with different working directories
// (and the server searches both ./ and ./data/ for its config.json), so
// resolving against the process CWD would let the two read different files:
// exactly the disagreement this shared loader exists to prevent. Absolute
// paths are returned untouched.
func ResolvePath(path, configPath string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	dir := filepath.Dir(configPath)
	if dir == "" || dir == "." {
		return path
	}
	return filepath.Join(dir, path)
}

// Load returns the effective region-scope names for one config: the union
// of the inline hashRegions list and, when hashRegionsPath is configured,
// the JSON array of names it points at.
//
// A missing or malformed file yields an error AND a usable Result — Names
// falls back to the inline list. That lets a startup caller log and carry
// on while the ingestor's SIGHUP reload propagates the error and keeps the
// keys it already has, matching the existing reload-failure behaviour.
//
// logf, when non-nil, gets one line per fact (file read, read failure,
// blank entries, duplicates) and never one line per entry: a real
// deployment configures ~1100 regions, and per-entry logging would bury
// the reload it is supposed to explain.
func Load(inline []string, cfgPath, configPath string, logf func(string, ...any)) (Result, error) {
	res := Result{Path: ResolvePath(ConfiguredPath(cfgPath), configPath)}

	var err error
	if res.Path != "" {
		res.FileNames, err = readNameFile(res.Path)
		if err != nil {
			res.FileNames = nil
		}
	}
	res.Names, res.Duplicates, res.Blank = mergeStats(inline, res.FileNames)

	if logf != nil {
		switch {
		case err != nil:
			logf("[regions] hashRegionsPath %s unusable, falling back to the inline hashRegions list: %v", res.Path, err)
		case res.Path != "":
			logf("[regions] %d region name(s) read from %s", len(res.FileNames), res.Path)
		}
		if res.Blank > 0 {
			logf("[regions] %d blank hashRegions entry/entries skipped", res.Blank)
		}
		if len(res.Duplicates) > 0 {
			logf("[regions] %d duplicate hashRegions entry/entries ignored (first: %q)", len(res.Duplicates), res.Duplicates[0])
		}
	}
	return res, err
}

// Merge is the union rule: inline entries normalized first, then the file's,
// deduplicated after normalization with first-seen order preserved. With no
// file entries it is exactly NormalizeNames(inline), so an inline-only
// deployment behaves as it always has.
func Merge(inline, file []string) []string {
	names, _, _ := mergeStats(inline, file)
	return names
}

// mergeStats is Merge plus the counters Load reports.
func mergeStats(inline, file []string) (names, duplicates []string, blank int) {
	seen := make(map[string]bool, len(inline)+len(file))
	dupeSeen := make(map[string]bool)
	names = make([]string, 0, len(inline)+len(file))
	for _, list := range [2][]string{inline, file} {
		for _, raw := range list {
			name, ok := Normalize(raw)
			if !ok {
				blank++
				continue
			}
			if seen[name] {
				if !dupeSeen[name] {
					dupeSeen[name] = true
					duplicates = append(duplicates, name)
				}
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, duplicates, blank
}

// readNameFile reads a JSON array of region names. Anything else — an
// object, numbers, truncated JSON — is an error rather than a silently
// empty list, so a typo in the file cannot quietly unconfigure every
// region.
func readNameFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return names, nil
}
