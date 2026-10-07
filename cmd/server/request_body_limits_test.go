package main

import (
	"encoding/json"
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
		"`413` — Request body over that endpoint's byte cap",
		fmt.Sprintf("| `POST /api/decode` | %d bytes |", maxDecodeBodyBytes),
		fmt.Sprintf("| `POST /api/packets/observations` | %d bytes |", maxBatchObservationsBodyBytes),
		fmt.Sprintf(`{ "error": "request body too large (max %d bytes)" }`, maxDecodeBodyBytes),
		fmt.Sprintf(`{ "error": "request body too large (max %d bytes)" }`, maxBatchObservationsBodyBytes),
		"## POST /api/packets/observations",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api-spec.md does not document %q", want)
		}
	}
}
