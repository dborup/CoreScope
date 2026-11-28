package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// Issue #334: POST /api/decode and POST /api/packets/observations decoded
// their JSON bodies with no byte cap. These tests pin the byte caps applied
// BEFORE parsing, the documented 413, and the fact that the pre-existing
// decode/200-hash behaviour is unchanged.

// padJSONTo returns obj (a complete JSON object) padded with trailing spaces
// to exactly n bytes. Trailing whitespace is valid JSON, so the padding only
// grows the body — it never changes the parsed value. Used to hit the byte
// boundaries exactly without depending on JSON field sizes.
func padJSONTo(t *testing.T, obj string, n int) string {
	t.Helper()
	if len(obj) > n {
		t.Fatalf("object %q is already %d bytes, cannot pad to %d", obj, len(obj), n)
	}
	return obj + strings.Repeat(" ", n-len(obj))
}

// postBody issues POST path with the given body through router and returns the
// recorder. Body is a strings.Reader, so ContentLength is set (the normal case).
func postBody(router http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// postBodyUnknownLength issues the same request with ContentLength == -1, the
// shape Go's server produces for a chunked (Transfer-Encoding: chunked) request.
// opaqueReader hides the concrete type so httptest.NewRequest cannot infer a
// length.
type opaqueReader struct{ r io.Reader }

func (o opaqueReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func postBodyUnknownLength(router http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, opaqueReader{strings.NewReader(body)})
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// assertJSONError asserts the response is the shared writeError shape
// ({"error": "..."}, application/json) with a non-empty message.
func assertJSONError(t *testing.T, w *httptest.ResponseRecorder, wantCode int) string {
	t.Helper()
	if w.Code != wantCode {
		t.Fatalf("status: want %d, got %d (body %q)", wantCode, w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type: want application/json, got %q", ct)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, w.Body.String())
	}
	if payload.Error == "" {
		t.Fatalf("expected a non-empty error field, got %q", w.Body.String())
	}
	return payload.Error
}

// --- POST /api/decode ---

func TestDecodeBodyLimitBoundaries(t *testing.T) {
	_, router := setupNoStoreServer(t)
	const obj = `{"hex":"0200"}`

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		w := postBody(router, "/api/decode", padJSONTo(t, obj, maxDecodeBodyBytes))
		if w.Code != http.StatusOK {
			t.Fatalf("want 200 for a %d-byte body, got %d (body %q)", maxDecodeBodyBytes, w.Code, w.Body.String())
		}
	})

	t.Run("one byte over the limit is 413", func(t *testing.T) {
		w := postBody(router, "/api/decode", padJSONTo(t, obj, maxDecodeBodyBytes+1))
		msg := assertJSONError(t, w, http.StatusRequestEntityTooLarge)
		if !strings.Contains(msg, fmt.Sprintf("%d", maxDecodeBodyBytes)) {
			t.Errorf("error message should name the limit, got %q", msg)
		}
	})

	t.Run("one byte under the limit is accepted", func(t *testing.T) {
		w := postBody(router, "/api/decode", padJSONTo(t, obj, maxDecodeBodyBytes-1))
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (body %q)", w.Code, w.Body.String())
		}
	})
}

// An oversized body whose bulk sits in a field the handler does not know about
// must still be rejected on bytes — the cap runs before parsing, so unknown
// fields cannot buy extra room.
func TestDecodeBodyLimitOversizedUnknownField(t *testing.T) {
	_, router := setupNoStoreServer(t)
	junk := strings.Repeat("A", maxDecodeBodyBytes)
	body := `{"hex":"0200","unknownField":"` + junk + `"}`
	if len(body) <= maxDecodeBodyBytes {
		t.Fatalf("test body is only %d bytes, expected > %d", len(body), maxDecodeBodyBytes)
	}
	w := postBody(router, "/api/decode", body)
	assertJSONError(t, w, http.StatusRequestEntityTooLarge)
}

func TestDecodeBodyLimitUnknownLength(t *testing.T) {
	_, router := setupNoStoreServer(t)
	const obj = `{"hex":"0200"}`

	t.Run("oversized with no Content-Length is 413", func(t *testing.T) {
		w := postBodyUnknownLength(router, "/api/decode", padJSONTo(t, obj, maxDecodeBodyBytes+1))
		assertJSONError(t, w, http.StatusRequestEntityTooLarge)
	})

	t.Run("a lying Content-Length cannot raise the cap", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/decode", opaqueReader{strings.NewReader(padJSONTo(t, obj, maxDecodeBodyBytes+1))})
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(obj)) // understates the real body
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assertJSONError(t, w, http.StatusRequestEntityTooLarge)
	})

	t.Run("within the cap with no Content-Length still decodes", func(t *testing.T) {
		w := postBodyUnknownLength(router, "/api/decode", obj)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (body %q)", w.Code, w.Body.String())
		}
	})
}

func TestDecodeBodyLimitMalformedJSON(t *testing.T) {
	_, router := setupNoStoreServer(t)

	t.Run("malformed under the cap is 400", func(t *testing.T) {
		w := postBody(router, "/api/decode", `{"hex":`)
		assertJSONError(t, w, http.StatusBadRequest)
	})

	t.Run("malformed over the cap is 413, not 400", func(t *testing.T) {
		// The cap runs before parsing, so bytes win over syntax.
		w := postBody(router, "/api/decode", `{"hex":"`+strings.Repeat("0", maxDecodeBodyBytes+1))
		assertJSONError(t, w, http.StatusRequestEntityTooLarge)
	})

	t.Run("empty body is 400", func(t *testing.T) {
		w := postBody(router, "/api/decode", "")
		assertJSONError(t, w, http.StatusBadRequest)
	})
}

// The cap must leave every frame the decoder can possibly accept decodable:
// 1 header + 4 transport codes + 1 path byte + MAX_PATH_SIZE path bytes +
// MAX_PACKET_PAYLOAD payload bytes, hex-encoded, wrapped in the JSON object.
func TestDecodeBodyLimitFitsLargestDecodableFrame(t *testing.T) {
	maxFrameBytes := 1 + 4 + 1 + maxPathSize + maxPacketPayload
	largest := len(`{"hex":""}`) + 2*maxFrameBytes
	if largest > maxDecodeBodyBytes {
		t.Fatalf("largest legitimate decode body is %d bytes, over the %d-byte cap", largest, maxDecodeBodyBytes)
	}
	if maxDecodeBodyBytes < 4*largest {
		t.Errorf("cap %d leaves less than 4x headroom over the largest legitimate body (%d)", maxDecodeBodyBytes, largest)
	}
	// Headroom is bounded on both sides: a cap orders of magnitude above what
	// any client can legitimately send is not a bound worth having.
	if maxDecodeBodyBytes > 32*largest {
		t.Errorf("cap %d is more than 32x the largest legitimate body (%d) — too loose to bound anything", maxDecodeBodyBytes, largest)
	}
}

func TestDecodeNormalRequestUnchanged(t *testing.T) {
	_, router := setupNoStoreServer(t)

	t.Run("a valid frame still decodes", func(t *testing.T) {
		w := postBody(router, "/api/decode", `{"hex":"0200"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (body %q)", w.Code, w.Body.String())
		}
		var resp struct {
			Decoded map[string]json.RawMessage `json:"decoded"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v (body %q)", err, w.Body.String())
		}
		for _, key := range []string{"header", "path", "payload"} {
			if _, ok := resp.Decoded[key]; !ok {
				t.Errorf("decoded.%s missing from %q", key, w.Body.String())
			}
		}
	})

	t.Run("missing hex is still 400", func(t *testing.T) {
		w := postBody(router, "/api/decode", `{"hex":""}`)
		if msg := assertJSONError(t, w, http.StatusBadRequest); msg != "hex is required" {
			t.Errorf("want \"hex is required\", got %q", msg)
		}
	})

	t.Run("undecodable hex is still 400", func(t *testing.T) {
		w := postBody(router, "/api/decode", `{"hex":"zz"}`)
		assertJSONError(t, w, http.StatusBadRequest)
	})
}

// --- POST /api/packets/observations ---

// batchBody returns {"hashes":[...]} with n distinct 16-hex hashes.
func batchBody(t *testing.T, n int) string {
	t.Helper()
	hashes := make([]string, n)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("%016x", i)
	}
	raw, err := json.Marshal(struct {
		Hashes []string `json:"hashes"`
	}{hashes})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestBatchObservationsBodyLimitBoundaries(t *testing.T) {
	_, router := setupNoStoreServer(t)
	const obj = `{"hashes":[]}`

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		w := postBody(router, "/api/packets/observations", padJSONTo(t, obj, maxBatchObservationsBodyBytes))
		if w.Code != http.StatusOK {
			t.Fatalf("want 200 for a %d-byte body, got %d (body %q)", maxBatchObservationsBodyBytes, w.Code, w.Body.String())
		}
	})

	t.Run("one byte over the limit is 413", func(t *testing.T) {
		w := postBody(router, "/api/packets/observations", padJSONTo(t, obj, maxBatchObservationsBodyBytes+1))
		msg := assertJSONError(t, w, http.StatusRequestEntityTooLarge)
		if !strings.Contains(msg, fmt.Sprintf("%d", maxBatchObservationsBodyBytes)) {
			t.Errorf("error message should name the limit, got %q", msg)
		}
	})

	t.Run("one byte under the limit is accepted", func(t *testing.T) {
		w := postBody(router, "/api/packets/observations", padJSONTo(t, obj, maxBatchObservationsBodyBytes-1))
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (body %q)", w.Code, w.Body.String())
		}
	})
}

func TestBatchObservationsBodyLimitOversizedUnknownField(t *testing.T) {
	_, router := setupNoStoreServer(t)
	junk := strings.Repeat("A", maxBatchObservationsBodyBytes)
	body := `{"hashes":[],"unknownField":"` + junk + `"}`
	w := postBody(router, "/api/packets/observations", body)
	assertJSONError(t, w, http.StatusRequestEntityTooLarge)
}

func TestBatchObservationsBodyLimitUnknownLength(t *testing.T) {
	_, router := setupNoStoreServer(t)

	t.Run("oversized with no Content-Length is 413", func(t *testing.T) {
		w := postBodyUnknownLength(router, "/api/packets/observations", padJSONTo(t, `{"hashes":[]}`, maxBatchObservationsBodyBytes+1))
		assertJSONError(t, w, http.StatusRequestEntityTooLarge)
	})

	t.Run("within the cap with no Content-Length still works", func(t *testing.T) {
		w := postBodyUnknownLength(router, "/api/packets/observations", batchBody(t, 2))
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (body %q)", w.Code, w.Body.String())
		}
	})
}

func TestBatchObservationsBodyLimitMalformedJSON(t *testing.T) {
	_, router := setupNoStoreServer(t)

	t.Run("malformed under the cap is 400", func(t *testing.T) {
		w := postBody(router, "/api/packets/observations", `not json`)
		assertJSONError(t, w, http.StatusBadRequest)
	})

	t.Run("malformed over the cap is 413, not 400", func(t *testing.T) {
		w := postBody(router, "/api/packets/observations", `{"hashes":["`+strings.Repeat("a", maxBatchObservationsBodyBytes+1))
		assertJSONError(t, w, http.StatusRequestEntityTooLarge)
	})
}

// The 200-hash limit is a cardinality limit and must keep firing on its own:
// the largest request it accepts has to stay well inside the byte cap, and a
// 201-hash request must still be the 400 it has always been, not a 413.
func TestBatchObservationsHashLimitPreserved(t *testing.T) {
	_, router := setupNoStoreServer(t)

	t.Run("200 hashes fit inside the byte cap", func(t *testing.T) {
		body := batchBody(t, 200)
		if len(body) > maxBatchObservationsBodyBytes {
			t.Fatalf("a 200-hash body is %d bytes, over the %d-byte cap", len(body), maxBatchObservationsBodyBytes)
		}
		if maxBatchObservationsBodyBytes < 4*len(body) {
			t.Errorf("cap %d leaves less than 4x headroom over a full 200-hash body (%d)", maxBatchObservationsBodyBytes, len(body))
		}
		if maxBatchObservationsBodyBytes > 64*len(body) {
			t.Errorf("cap %d is more than 64x a full 200-hash body (%d) — too loose to bound anything", maxBatchObservationsBodyBytes, len(body))
		}
		w := postBody(router, "/api/packets/observations", body)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (body %q)", w.Code, w.Body.String())
		}
	})

	t.Run("201 hashes is still 400, not 413", func(t *testing.T) {
		body := batchBody(t, 201)
		if len(body) > maxBatchObservationsBodyBytes {
			t.Fatalf("a 201-hash body is %d bytes, so the byte cap would mask the hash limit", len(body))
		}
		msg := assertJSONError(t, postBody(router, "/api/packets/observations", body), http.StatusBadRequest)
		if !strings.Contains(msg, "too many hashes") {
			t.Errorf("want the hash-limit message, got %q", msg)
		}
	})
}

func TestBatchObservationsNormalRequestUnchanged(t *testing.T) {
	_, router := setupNoStoreServer(t)

	t.Run("empty hashes returns an empty results map", func(t *testing.T) {
		w := postBody(router, "/api/packets/observations", `{"hashes":[]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (body %q)", w.Code, w.Body.String())
		}
		var resp struct {
			Results map[string]json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v (body %q)", err, w.Body.String())
		}
		if len(resp.Results) != 0 {
			t.Errorf("want an empty results map, got %v", resp.Results)
		}
	})

	t.Run("known hashes return a results map", func(t *testing.T) {
		w := postBody(router, "/api/packets/observations", `{"hashes":["abc123","def456"]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (body %q)", w.Code, w.Body.String())
		}
		var resp struct {
			Results map[string]json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v (body %q)", err, w.Body.String())
		}
	})
}

// --- Real HTTP, real chunked transfer encoding ---

// Go's http.Client sends a request with an unknown-length body as
// Transfer-Encoding: chunked, so this exercises the limit over a real
// connection with no Content-Length at all — the case a Content-Length
// precheck alone would miss.
func TestBodyLimitsOverRealChunkedRequests(t *testing.T) {
	_, router := setupNoStoreServer(t)
	ts := httptest.NewServer(router)
	defer ts.Close()

	post := func(t *testing.T, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("POST", ts.URL+path, opaqueReader{strings.NewReader(body)})
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		return resp
	}

	cases := []struct {
		name     string
		path     string
		body     string
		wantCode int
	}{
		{"decode within the cap", "/api/decode", `{"hex":"0200"}`, http.StatusOK},
		{"decode over the cap", "/api/decode", padJSONTo(t, `{"hex":"0200"}`, maxDecodeBodyBytes+1), http.StatusRequestEntityTooLarge},
		{"batch within the cap", "/api/packets/observations", batchBody(t, 3), http.StatusOK},
		{"batch over the cap", "/api/packets/observations", padJSONTo(t, `{"hashes":[]}`, maxBatchObservationsBodyBytes+1), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(t, tc.path, tc.body)
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status: want %d, got %d (body %q)", tc.wantCode, resp.StatusCode, raw)
			}
			if resp.StatusCode == http.StatusRequestEntityTooLarge {
				if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Errorf("Content-Type: want application/json, got %q", ct)
				}
				var payload struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(raw, &payload); err != nil || payload.Error == "" {
					t.Errorf("413 body is not the shared error shape: %q (%v)", raw, err)
				}
			}
		})
	}
}

// The request side really is chunked, i.e. the server sees no Content-Length.
func TestChunkedRequestHasNoContentLength(t *testing.T) {
	var seen int64 = -2
	var te []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.ContentLength
		te = r.TransferEncoding
		io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()

	req, err := http.NewRequest("POST", srv.URL, opaqueReader{strings.NewReader(`{"hex":"0200"}`)})
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if seen != -1 {
		t.Errorf("server saw ContentLength %d, want -1 (unknown)", seen)
	}
	if len(te) != 1 || te[0] != "chunked" {
		t.Errorf("server saw TransferEncoding %v, want [chunked]", te)
	}
}

// --- Documentation ---

// The 413 has to be documented, not just implemented: the served OpenAPI spec
// must declare it on both routes, with the shared ApiError body.
func TestOpenAPIDocuments413OnBodyLimitedRoutes(t *testing.T) {
	router := mux.NewRouter()
	srv, _ := setupNoStoreServer(t)
	srv.RegisterRoutes(router)
	spec := buildOpenAPISpec(router, "test")

	paths, ok := spec["paths"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec has no paths object")
	}
	schemas, _ := spec["components"].(map[string]interface{})["schemas"].(map[string]interface{})
	if _, ok := schemas["ApiError"]; !ok {
		t.Errorf("components/schemas is missing ApiError")
	}

	for _, path := range []string{"/api/decode", "/api/packets/observations"} {
		item, ok := paths[path].(map[string]interface{})
		if !ok {
			t.Errorf("%s missing from the spec", path)
			continue
		}
		op, ok := item["post"].(map[string]interface{})
		if !ok {
			t.Errorf("%s has no post operation", path)
			continue
		}
		resps, ok := op["responses"].(map[string]interface{})
		if !ok {
			t.Errorf("%s post has no responses", path)
			continue
		}
		resp, ok := resps["413"].(openAPIResponse)
		if !ok {
			documented := keysOf(resps)
			sort.Strings(documented)
			t.Errorf("%s post does not document a 413 (has %v)", path, documented)
			continue
		}
		if resp.Description == "" {
			t.Errorf("%s post 413 has no description", path)
		}
		media, ok := resp.Content["application/json"]
		if !ok || media.Schema == nil || media.Schema.Ref != "#/components/schemas/ApiError" {
			t.Errorf("%s post 413 does not reference ApiError, got %+v", path, resp.Content)
		}
	}
}

// The caps are an external contract, so docs/api-spec.md has to name both of
// them and the 413 — a reader must not have to read the Go source to find out
// how big a body they may send.
func TestAPISpecDocumentsBodyLimits(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api-spec.md")
	if err != nil {
		t.Fatalf("read api-spec.md: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"Request-body byte caps",
		"`413` — Request body over the byte cap on `POST /api/decode` or `POST /api/packets/observations`",
		fmt.Sprintf("| `POST /api/decode` | %d bytes | bytes received, before parsing | `413`", maxDecodeBodyBytes),
		fmt.Sprintf("| `POST /api/packets/observations` | %d bytes | bytes received, before parsing | `413`", maxBatchObservationsBodyBytes),
		fmt.Sprintf(`{ "error": "request body too large (max %d bytes)" }`, maxDecodeBodyBytes),
		fmt.Sprintf(`{ "error": "request body too large (max %d bytes)" }`, maxBatchObservationsBodyBytes),
		"## POST /api/packets/observations",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api-spec.md does not document %q", want)
		}
	}
}

// The caps table has to describe the endpoints it lists as they actually
// behave: the right route names, the right caps, and the right status code —
// including the endpoints that answer 400 rather than 413. A table that
// overclaims is worse than no table, because a client trusts it.
func TestAPISpecCapsTableMatchesRealBehaviour(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api-spec.md")
	if err != nil {
		t.Fatalf("read api-spec.md: %v", err)
	}
	doc := string(raw)

	for _, want := range []string{
		// Endpoints capped before #334: still 400, and said so.
		fmt.Sprintf("| `POST /api/paths/inspect` | %d bytes | the streaming decoder | `400`", inspectBodyLimit),
		fmt.Sprintf("| `POST /api/channel-proposals` | %d bytes | the streaming decoder | `400`", maxProposalBodyBytes),
		fmt.Sprintf("| `PUT /api/config/geo-filter` (API key) | %d bytes (1 MiB) | the streaming decoder | `400`", geoFilterBodyLimit),
		// The one body-taking route with no cap at all.
		"`POST /api/admin/prune-geo-filter?confirm=true`",
		"**no limit at all**",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api-spec.md does not document %q", want)
		}
	}

	// /api/path-inspect is not a route; it 404s. The real path is
	// /api/paths/inspect.
	if strings.Contains(doc, "/api/path-inspect") {
		t.Errorf("docs/api-spec.md still names the non-existent route /api/path-inspect")
	}
}

// info.description is the first thing an API consumer reads. It must not claim
// a blanket 413 the implementation does not give (#334 review F2).
func TestOpenAPIDescriptionDoesNotOverclaim413(t *testing.T) {
	router := mux.NewRouter()
	srv, _ := setupNoStoreServer(t)
	srv.RegisterRoutes(router)
	spec := buildOpenAPISpec(router, "test")

	info, _ := spec["info"].(map[string]interface{})
	desc, _ := info["description"].(string)
	if desc == "" {
		t.Fatalf("spec info has no description")
	}
	for _, want := range []string{
		"POST /api/decode and POST /api/packets/observations cap their request body",
		"413",
		"bound their body on the JSON decoder instead",
		"reports an over-cap body as 400",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("info.description does not say %q; got %q", want, desc)
		}
	}
	if strings.Contains(desc, "Endpoints that take a request body cap it in bytes before parsing it and answer 413") {
		t.Errorf("info.description still claims every body-taking endpoint answers 413: %q", desc)
	}
}

// The summary is what shows up in the route list, so it must not contradict the
// description: this route returns observations, it does not submit them
// (#334 review F5).
func TestBatchObservationsSummaryMatchesBehaviour(t *testing.T) {
	meta, ok := routeDescriptions()["POST /api/packets/observations"]
	if !ok {
		t.Fatalf("POST /api/packets/observations has no route metadata")
	}
	if strings.Contains(strings.ToLower(meta.Summary), "submit") {
		t.Errorf("summary %q says the route submits observations; it returns them", meta.Summary)
	}
	if !strings.Contains(strings.ToLower(meta.Summary), "observations") {
		t.Errorf("summary %q does not mention observations", meta.Summary)
	}
}

// --- The cap must bound what the handler READS, not just what it answers ---
//
// A handler that reads the whole body and only then checks its size answers
// exactly the same 413 as one that stops at the cap, so no status-code
// assertion can tell the two apart — and the second one is the memory
// regression #334 exists to prevent. The tests below therefore assert on the
// bytes the handler pulled off the body, which is the actual property.

// errEndlessBodyHardStop aborts a runaway read instead of letting the test hang
// or allocate without bound. Reaching it is itself a failure signal.
var errEndlessBodyHardStop = errors.New("endless body hard stop reached")

const (
	// endlessBodyHardStop is how far a handler is allowed to get before the
	// body gives up on it. Orders of magnitude above both caps, so only a
	// handler with no bound at all can reach it.
	endlessBodyHardStop = 8 << 20 // 8 MiB

	// bodyReadSlack is the margin allowed on top of the cap. http.MaxBytesReader
	// reads at most limit+1 bytes from the underlying body (it trims each Read
	// to the remaining budget plus one byte, which is all it needs to know the
	// limit was passed), so one page of slack is generous while still being
	// ~2000x below endlessBodyHardStop.
	bodyReadSlack = 4 << 10
)

// countingEndlessBody is a request body that never ends: a single '{' followed
// by spaces forever. It counts every byte it hands out.
type countingEndlessBody struct {
	read    int64
	started bool
}

func (c *countingEndlessBody) Read(p []byte) (int, error) {
	if c.read >= endlessBodyHardStop {
		return 0, errEndlessBodyHardStop
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := len(p)
	if remaining := endlessBodyHardStop - c.read; int64(n) > remaining {
		n = int(remaining)
	}
	for i := 0; i < n; i++ {
		p[i] = ' '
	}
	if !c.started {
		// Looks like the start of a JSON object, so a handler that streams the
		// body into a decoder keeps asking for more rather than failing early.
		p[0] = '{'
		c.started = true
	}
	c.read += int64(n)
	return n, nil
}

func (c *countingEndlessBody) Close() error { return nil }

// countingReader counts the bytes read from a finite body.
type countingReader struct {
	r    io.Reader
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingReader) Close() error { return nil }

// bodyLimitedEndpoints is the set of routes decodeLimitedJSONBody guards, with
// the cap each one enforces.
func bodyLimitedEndpoints() []struct {
	path  string
	limit int64
} {
	return []struct {
		path  string
		limit int64
	}{
		{"/api/decode", maxDecodeBodyBytes},
		{"/api/packets/observations", maxBatchObservationsBodyBytes},
	}
}

// An endless body must be cut off at the cap. A handler that buffers the whole
// request before measuring it would keep pulling until the hard stop.
func TestBodyLimitsStopReadingAtTheCap(t *testing.T) {
	_, router := setupNoStoreServer(t)

	for _, ep := range bodyLimitedEndpoints() {
		t.Run(ep.path, func(t *testing.T) {
			body := &countingEndlessBody{}
			req := httptest.NewRequest("POST", ep.path, body)
			req.Header.Set("Content-Type", "application/json")
			req.ContentLength = -1 // as a chunked request arrives
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if body.read > ep.limit+bodyReadSlack {
				t.Errorf("handler read %d bytes of an endless body; the cap is %d (allowed at most %d). "+
					"The byte cap has to be enforced while reading, not checked after the body is already buffered.",
					body.read, ep.limit, ep.limit+bodyReadSlack)
			}
			assertJSONError(t, w, http.StatusRequestEntityTooLarge)
		})
	}
}

// The same property on a finite body: a request far over the cap must not be
// drained in full before it is rejected.
func TestBodyLimitsDoNotDrainAnOversizedBody(t *testing.T) {
	_, router := setupNoStoreServer(t)

	for _, ep := range bodyLimitedEndpoints() {
		t.Run(ep.path, func(t *testing.T) {
			raw := `{"x":"` + strings.Repeat("A", int(16*ep.limit)) + `"}`
			body := &countingReader{r: strings.NewReader(raw)}
			req := httptest.NewRequest("POST", ep.path, body)
			req.Header.Set("Content-Type", "application/json")
			req.ContentLength = int64(len(raw))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if body.read > ep.limit+bodyReadSlack {
				t.Errorf("handler read %d bytes of a %d-byte body; the cap is %d (allowed at most %d)",
					body.read, len(raw), ep.limit, ep.limit+bodyReadSlack)
			}
			assertJSONError(t, w, http.StatusRequestEntityTooLarge)
		})
	}
}

// --- Trailing data after the JSON body (#334 F3) ---
//
// Deliberate behaviour change: json.Unmarshal rejects anything but whitespace
// after the JSON value, while the json.Decoder.Decode these handlers used
// before silently ignored it. That matches decodeSingleJSONObject on
// POST /api/channel-proposals, and the frontend only ever sends
// JSON.stringify output, so nothing legitimate regresses. Pinned here so it
// stays a decision rather than drifting back.
func TestBodyLimitsRejectTrailingData(t *testing.T) {
	_, router := setupNoStoreServer(t)

	cases := []struct {
		name string
		path string
		body string
	}{
		{"decode: junk after the object", "/api/decode", `{"hex":"0200"} trailing`},
		{"decode: a second JSON object", "/api/decode", `{"hex":"0200"}{"hex":"zz"}`},
		{"batch: junk after the object", "/api/packets/observations", `{"hashes":[]} junk`},
		{"batch: a second JSON object", "/api/packets/observations", `{"hashes":[]}{"hashes":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertJSONError(t, postBody(router, tc.path, tc.body), http.StatusBadRequest)
		})
	}

	// Trailing whitespace is valid JSON and must still be accepted — that is
	// what the boundary tests pad with.
	t.Run("trailing whitespace is still accepted", func(t *testing.T) {
		w := postBody(router, "/api/decode", `{"hex":"0200"}`+"\n\t  ")
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (body %q)", w.Code, w.Body.String())
		}
	})
}
