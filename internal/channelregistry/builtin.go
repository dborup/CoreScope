package channelregistry

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BuiltinNamesFileName is the file, inside the queue directory, where the
// ingestor publishes the hashtag channel names its config-derived key layer
// already decrypts: the built-in keys, the rainbow table
// (channel-rainbow.json, ~320 common names such as #test and #chat),
// hashChannels and channelKeys. That layer wins over approved suggestions, so
// approving such a name adds nothing and revoking it does not stop its
// traffic from being decrypted. The read-only server uses the file to say so
// in the admin list and in a submitter's request status.
const BuiltinNamesFileName = "builtin-channels.json"

// MaxBuiltinNames bounds the published set. The shipped rainbow table has
// about 320 names; operators can add more through config.
const MaxBuiltinNames = 4096

// maxBuiltinNamesBytes bounds how much of the file a reader parses:
// MaxBuiltinNames names of at most MaxNameBytes bytes, JSON-quoted.
const maxBuiltinNamesBytes = MaxBuiltinNames * (MaxNameBytes*6 + 4)

type builtinNamesFile struct {
	Names []string `json:"names"`
}

// BuiltinNamesPath returns the path of the builtin names file.
func (q *Queue) BuiltinNamesPath() string {
	return filepath.Join(q.dir, BuiltinNamesFileName)
}

// WriteBuiltinNames atomically replaces the published set with the distinct
// '#' names in names (other keys, such as "Public" or PSK channels, cannot be
// suggested and are left out), sorted and capped at MaxBuiltinNames.
func (q *Queue) WriteBuiltinNames(names []string) error {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if len(n) < 2 || !strings.HasPrefix(n, "#") {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	if len(out) > MaxBuiltinNames {
		out = out[:MaxBuiltinNames]
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := os.MkdirAll(q.dir, 0o755); err != nil {
		return err
	}
	return q.writeAtomic(q.BuiltinNamesPath(), builtinNamesFile{Names: out})
}

// ReadBuiltinNames returns the published set. A missing file (an ingestor
// that has not written it yet) is an empty set, not an error.
func (q *Queue) ReadBuiltinNames() (map[string]bool, error) {
	f, err := os.Open(q.BuiltinNamesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBuiltinNamesBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBuiltinNamesBytes {
		return nil, errors.New("builtin channel names file is too large")
	}
	var file builtinNamesFile
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, err
	}
	if len(file.Names) > MaxBuiltinNames {
		file.Names = file.Names[:MaxBuiltinNames]
	}
	set := make(map[string]bool, len(file.Names))
	for _, n := range file.Names {
		set[n] = true
	}
	return set, nil
}
