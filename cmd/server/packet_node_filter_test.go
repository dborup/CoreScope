package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// #335: GET /api/packets?nodes= is bounded and normalised before any node
// lookup, query or packet-store lock.

const seededNodeKey = "aabbccdd11223344" // seedTestData: tx 1 (newest) and tx 3

// distinctNodeEntries returns n distinct entries whose first is the seeded key,
// so every valid list must return exactly the seeded node's packets.
func distinctNodeEntries(n int) []string {
	out := []string{seededNodeKey}
	for i := 1; i < n; i++ {
		out = append(out, fmt.Sprintf("%064x", i))
	}
	return out
}

func TestParsePacketNodeFilters(t *testing.T) {
	ok := func(name, raw string, want []string) {
		t.Run(name, func(t *testing.T) {
			got, err := parsePacketNodeFilters(raw)
			if err != nil {
				t.Fatalf("parsePacketNodeFilters(%q) error: %v", raw, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parsePacketNodeFilters(%.60q) = %d entries %.120q, want %d entries", raw, len(got), got, len(want))
			}
		})
	}
	bad := func(name, raw, wantMsg string) {
		t.Run(name, func(t *testing.T) {
			got, err := parsePacketNodeFilters(raw)
			if err == nil {
				t.Fatalf("parsePacketNodeFilters(%d bytes) = %d entries, want error", len(raw), len(got))
			}
			if !strings.Contains(err.Error(), wantMsg) {
				t.Fatalf("error %q does not contain %q", err, wantMsg)
			}
		})
	}

	ok("single", "abc", []string{"abc"})
	ok("whitespace trimmed", "  abc ,\tdef\n", []string{"abc", "def"})
	ok("empty entries dropped", ",,abc,, ,def,", []string{"abc", "def"})
	ok("only empty entries", " , ,", []string{})
	ok("duplicates collapse, first order kept", "def,abc, def ,abc", []string{"def", "abc"})
	// Matching is exact (public_key = ? OR name = ?), so case variants are
	// different entries and are neither folded nor collapsed.
	ok("mixed case kept distinct", "AbC,abc,ABC,abc", []string{"AbC", "abc", "ABC"})

	limitMinus1 := distinctNodeEntries(maxPacketNodeFilters - 1)
	ok("limit-1 distinct", strings.Join(limitMinus1, ","), limitMinus1)
	atLimit := distinctNodeEntries(maxPacketNodeFilters)
	ok("limit distinct", strings.Join(atLimit, ","), atLimit)
	bad("limit+1 distinct", strings.Join(distinctNodeEntries(maxPacketNodeFilters+1), ","),
		fmt.Sprintf("at most %d distinct", maxPacketNodeFilters))

	// Duplicates and empties do not count towards the distinct cap...
	withDups := append(append([]string{}, atLimit...), seededNodeKey, " "+atLimit[1]+" ", "", "")
	ok("limit distinct plus duplicates and empties", strings.Join(withDups, ","), atLimit)
	// ...but a case variant is one more distinct entry.
	bad("limit distinct plus a case variant", strings.Join(append(append([]string{}, atLimit...), strings.ToUpper(seededNodeKey)), ","),
		fmt.Sprintf("at most %d distinct", maxPacketNodeFilters))

	// Raw entry count, counted before splitting: duplicates still cost a
	// split and a trim each.
	ok("raw entries at limit", strings.Repeat("a,", maxPacketNodeFilterRawEntries-1)+"a", []string{"a"})
	bad("raw entries over limit", strings.Repeat("a,", maxPacketNodeFilterRawEntries)+"a",
		fmt.Sprintf("at most %d comma-separated", maxPacketNodeFilterRawEntries))
	bad("raw empty entries over limit", strings.Repeat(",", maxPacketNodeFilterRawEntries),
		fmt.Sprintf("at most %d comma-separated", maxPacketNodeFilterRawEntries))

	// Raw byte length, checked before anything else.
	ok("bytes at limit", strings.Repeat(" ", maxPacketNodeFilterBytes-3)+"abc", []string{"abc"})
	bad("bytes over limit", strings.Repeat(" ", maxPacketNodeFilterBytes-2)+"abc",
		fmt.Sprintf("at most %d characters", maxPacketNodeFilterBytes))
	bad("one huge entry", strings.Repeat("a", 1<<20), fmt.Sprintf("at most %d characters", maxPacketNodeFilterBytes))
}

// 50 full public keys with ", " separators must fit the byte bound, or the
// documented cardinality would be unreachable for a well-formed client.
func TestPacketNodeFilterBoundsAdmitFullKeys(t *testing.T) {
	full := make([]string, maxPacketNodeFilters)
	for i := range full {
		full[i] = fmt.Sprintf("%064x", i+1) // MeshCore.h PUB_KEY_SIZE 32 = 64 hex chars
	}
	raw := strings.Join(full, ", ")
	if len(raw) > maxPacketNodeFilterBytes {
		t.Fatalf("%d full keys are %d bytes, over the %d-byte bound", maxPacketNodeFilters, len(raw), maxPacketNodeFilterBytes)
	}
	got, err := parsePacketNodeFilters(raw)
	if err != nil || len(got) != maxPacketNodeFilters {
		t.Fatalf("parse of %d full keys: %d entries, err %v", maxPacketNodeFilters, len(got), err)
	}
	if maxPacketNodeFilterRawEntries < maxPacketNodeFilters {
		t.Fatalf("raw entry bound %d below the distinct cap %d", maxPacketNodeFilterRawEntries, maxPacketNodeFilters)
	}
}

type multiNodeBody struct {
	Packets []struct {
		Hash string `json:"hash"`
	} `json:"packets"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Error  string `json:"error"`
}

func getMultiNode(t *testing.T, router *mux.Router, query string) (int, multiNodeBody) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/packets?"+query, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var body multiNodeBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %.120s: invalid JSON %q: %v", query, w.Body.String(), err)
	}
	return w.Code, body
}

func nodesQuery(entries []string) string {
	return "nodes=" + url.QueryEscape(strings.Join(entries, ","))
}

func hashesOf(b multiNodeBody) []string {
	out := make([]string, 0, len(b.Packets))
	for _, p := range b.Packets {
		out = append(out, p.Hash)
	}
	return out
}

// Both the in-memory store and the DB fallback must answer identically.
func multiNodePaths(t *testing.T) map[string]*mux.Router {
	_, storeRouter := setupTestServer(t)
	_, dbRouter := setupNoStoreServer(t)
	return map[string]*mux.Router{"store": storeRouter, "db": dbRouter}
}

func TestHandlePacketsMultiNodeLimits(t *testing.T) {
	for name, router := range multiNodePaths(t) {
		t.Run(name, func(t *testing.T) {
			for _, n := range []int{maxPacketNodeFilters - 1, maxPacketNodeFilters} {
				code, body := getMultiNode(t, router, "nodes="+strings.Join(distinctNodeEntries(n), ","))
				if code != 200 || body.Total != 2 {
					t.Errorf("%d distinct entries: status %d total %d (%q), want 200 total 2", n, code, body.Total, body.Error)
				}
			}

			code, body := getMultiNode(t, router, "nodes="+strings.Join(distinctNodeEntries(maxPacketNodeFilters+1), ","))
			want := fmt.Sprintf("nodes accepts at most %d distinct entries", maxPacketNodeFilters)
			if code != 400 || body.Error != want {
				t.Errorf("limit+1: status %d error %q, want 400 %q", code, body.Error, want)
			}

			code, body = getMultiNode(t, router, "nodes="+strings.Repeat("a", maxPacketNodeFilterBytes+1))
			if code != 400 || !strings.Contains(body.Error, "characters") {
				t.Errorf("over-long nodes: status %d error %q, want 400", code, body.Error)
			}
			code, body = getMultiNode(t, router, "nodes="+strings.Repeat(seededNodeKey+",", maxPacketNodeFilterRawEntries+1))
			if code != 400 || !strings.Contains(body.Error, "comma-separated") {
				t.Errorf("too many raw entries: status %d error %q, want 400", code, body.Error)
			}
		})
	}
}

func TestHandlePacketsMultiNodeNormalisation(t *testing.T) {
	for name, router := range multiNodePaths(t) {
		t.Run(name, func(t *testing.T) {
			_, single := getMultiNode(t, router, "nodes="+seededNodeKey)
			if single.Total != 2 || len(single.Packets) != 2 {
				t.Fatalf("single node: total %d, %d packets, want 2", single.Total, len(single.Packets))
			}

			// Duplicates, empty entries and surrounding whitespace collapse
			// to the single-node answer, at the cap too.
			atLimit := distinctNodeEntries(maxPacketNodeFilters)
			for _, q := range []string{
				"nodes=%20" + seededNodeKey + "%20,," + seededNodeKey + ",%09,",
				nodesQuery(append(append([]string{}, atLimit...), seededNodeKey, "", " "+atLimit[1])),
			} {
				code, body := getMultiNode(t, router, q)
				if code != 200 || !reflect.DeepEqual(hashesOf(body), hashesOf(single)) || body.Total != single.Total {
					t.Errorf("%.80s...: status %d hashes %v total %d (%q), want %v total %d",
						q, code, hashesOf(body), body.Total, body.Error, hashesOf(single), single.Total)
				}
			}

			// Only-empty entries keep the pre-#335 answer: an empty page.
			code, body := getMultiNode(t, router, "nodes=,%20,")
			if code != 200 || body.Total != 0 || len(body.Packets) != 0 {
				t.Errorf("only empty entries: status %d total %d, want 200 total 0", code, body.Total)
			}

			// No case folding: an upper-case key matches nothing, exactly as
			// before; it is a separate entry, so it can push a list over the cap.
			code, body = getMultiNode(t, router, "nodes="+strings.ToUpper(seededNodeKey))
			if code != 200 || body.Total != 0 {
				t.Errorf("upper-case key: status %d total %d, want 200 total 0", code, body.Total)
			}
			code, body = getMultiNode(t, router, "nodes="+strings.Join(append(append([]string{}, atLimit...), strings.ToUpper(seededNodeKey)), ","))
			if code != 400 {
				t.Errorf("cap plus a case variant: status %d, want 400", code)
			}
		})
	}
}

// Normal filters keep their semantics: name resolution, multi-node union,
// ordering and pagination.
func TestHandlePacketsMultiNodeSemanticsUnchanged(t *testing.T) {
	for name, router := range multiNodePaths(t) {
		t.Run(name, func(t *testing.T) {
			// Pages are ordered by ingest id (#1345): tx 3 is newer than tx 1.
			const newer, older = "def456abc1230099", "abc123def4567890"
			cases := []struct {
				query      string
				wantHashes []string
				wantTotal  int
			}{
				{"nodes=" + seededNodeKey, []string{newer, older}, 2},
				{"nodes=TestRepeater", []string{newer, older}, 2},
				{"nodes=" + seededNodeKey + ",eeff00112233aabb", []string{newer, older}, 2},
				{"nodes=ffffffffffffffff", []string{}, 0},
				{"nodes=" + seededNodeKey + "&order=asc", []string{older, newer}, 2},
				{"nodes=" + seededNodeKey + "&limit=1", []string{newer}, 2},
				{"nodes=" + seededNodeKey + "&limit=1&offset=1", []string{older}, 2},
				{"nodes=" + seededNodeKey + "&order=asc&limit=1&offset=1", []string{newer}, 2},
				{"nodes=" + seededNodeKey + "&offset=2", []string{}, 2},
			}
			for _, c := range cases {
				code, body := getMultiNode(t, router, c.query)
				if code != 200 || !reflect.DeepEqual(hashesOf(body), c.wantHashes) || body.Total != c.wantTotal {
					t.Errorf("%s: status %d hashes %v total %d (%q), want %v total %d",
						c.query, code, hashesOf(body), body.Total, body.Error, c.wantHashes, c.wantTotal)
				}
			}
		})
	}
}

// The in-memory path resolves every entry under the packet store's read lock.
// An oversized list must be rejected without waiting for that lock.
func TestHandlePacketsMultiNodeRejectsBeforeStoreLock(t *testing.T) {
	srv, router := setupTestServer(t)
	srv.store.mu.Lock()
	locked := true
	defer func() {
		if locked {
			srv.store.mu.Unlock()
		}
	}()

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest("GET", "/api/packets?nodes="+strings.Join(distinctNodeEntries(maxPacketNodeFilters+1), ","), nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		done <- w.Code
	}()
	select {
	case code := <-done:
		if code != 400 {
			t.Fatalf("status %d, want 400", code)
		}
	case <-time.After(2 * time.Second):
		srv.store.mu.Unlock()
		locked = false
		<-done
		t.Fatal("oversized nodes= waited for the packet-store lock instead of failing fast")
	}
}

// The DB fallback must reject an oversized list before any SQL runs: with the
// connection closed, reaching a query would turn the answer into a 500.
func TestHandlePacketsMultiNodeRejectsBeforeDBQuery(t *testing.T) {
	srv, router := setupNoStoreServer(t)
	srv.db.conn.Close()
	code, body := getMultiNode(t, router, "nodes="+strings.Join(distinctNodeEntries(maxPacketNodeFilters+1), ","))
	if code != 400 {
		t.Fatalf("status %d (%q), want 400 before any query", code, body.Error)
	}
}

func TestOpenAPIDocumentsPacketNodeFilterLimit(t *testing.T) {
	router := mux.NewRouter()
	srv, _ := setupNoStoreServer(t)
	srv.RegisterRoutes(router)
	raw, err := json.Marshal(buildOpenAPISpec(router, "test"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"parameters"`
			Responses map[string]struct {
				Description string `json:"description"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	op := spec.Paths["/api/packets"]["get"]
	var nodesDesc string
	for _, p := range op.Parameters {
		if p.Name == "nodes" {
			nodesDesc = p.Description
		}
	}
	for _, want := range []string{
		fmt.Sprintf("At most %d distinct entries", maxPacketNodeFilters),
		fmt.Sprintf("%d comma-separated entries", maxPacketNodeFilterRawEntries),
		fmt.Sprintf("%d characters", maxPacketNodeFilterBytes),
		"400",
	} {
		if !strings.Contains(nodesDesc, want) {
			t.Errorf("OpenAPI nodes parameter %q does not mention %q", nodesDesc, want)
		}
	}
	if !strings.Contains(op.Responses["400"].Description, "nodes") {
		t.Errorf("GET /api/packets 400 response %q does not mention nodes", op.Responses["400"].Description)
	}
}

func TestAPISpecDocumentsPacketNodeFilterLimit(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api-spec.md")
	if err != nil {
		t.Fatalf("read api-spec.md: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		fmt.Sprintf("at most %d distinct entries", maxPacketNodeFilters),
		fmt.Sprintf("%d comma-separated entries", maxPacketNodeFilterRawEntries),
		fmt.Sprintf("%d characters", maxPacketNodeFilterBytes),
		fmt.Sprintf(`{ "error": "nodes accepts at most %d distinct entries" }`, maxPacketNodeFilters),
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api-spec.md does not document %q", want)
		}
	}
}
