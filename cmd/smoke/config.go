package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/meshcore-analyzer/dhm"
)

// tokenDHM is the config key of the Dataforsyningen token: DHM and the
// Skråfoto STAC search use it.
const tokenDHM = "map.dhm.token"

// tokens are the tokens read from the config, by config key; a missing
// or blank one is "".
type tokens map[string]string

// secrets are every non-empty token, for the redactor.
func (t tokens) secrets() []string {
	out := make([]string, 0, len(t))
	for _, v := range t {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// loadTokens reads the tokens from the server config at path the way the
// server reads its typed map blocks (cmd/server liftMapBlock): map.dhm,
// the exact key only, decoded into dhm.Config, the server's own type for
// the block. The server's LoadConfig lives in package main and cannot be
// imported. Errors name the file and the problem, never its content.
func loadTokens(path string) (tokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config %s: cannot be read", path)
	}
	var doc struct {
		Map map[string]json.RawMessage `json:"map"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("config %s: %s", path, describeDecodeError(err))
	}
	toks := tokens{tokenDHM: ""}
	raw, ok := doc.Map["dhm"]
	if !ok {
		return toks, nil
	}
	var c dhm.Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("config %s: map.dhm is malformed: %s", path, describeDecodeError(err))
	}
	toks[tokenDHM] = strings.TrimSpace(c.Token)
	return toks, nil
}

// describeDecodeError is the category of a JSON error and where it sits,
// never the text around it: the file holds the token.
func describeDecodeError(err error) string {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Sprintf("invalid JSON at byte offset %d", syntaxErr.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Sprintf("wrong JSON type for field %s", typeErr.Field)
	}
	return "it could not be decoded as JSON"
}
