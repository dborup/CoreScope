package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Issue #334 — request-body byte caps for the two public POST endpoints that
// used to hand r.Body straight to a json.Decoder with no bound:
//
//   - POST /api/decode              (maxDecodeBodyBytes)
//   - POST /api/packets/observations (maxBatchObservationsBodyBytes)
//
// Both caps run BEFORE any parsing, so an oversized body never gets decoded
// into memory, whatever its shape (deeply nested, one giant string, or bulk
// stuffed into a field the handler does not even know about). The existing
// 200-hash limit on the batch endpoint is a cardinality limit that runs after
// parsing, so it never bounded bytes; it is unchanged and still fires first for
// every request a legitimate client sends (see the headroom notes below).
//
// Do not rely on a reverse proxy for this: the caps are enforced in the
// application, and TestBodyLimitsOverRealChunkedRequests exercises them over a
// real connection with no Content-Length at all.
//
// TODO(#334, AGENTS.md rule 8): both limits are hardcoded here for now and
// should be exposed as operator-configurable values (config.json, and the
// customizer where it makes sense) in a later milestone.
const (
	// maxDecodeBodyBytes caps POST /api/decode. The largest frame DecodePacket
	// can possibly accept is 1 header + 4 transport codes + 1 path byte +
	// MAX_PATH_SIZE (64) path bytes + MAX_PACKET_PAYLOAD (184) payload bytes =
	// 254 bytes, i.e. 508 hex characters, i.e. a ~520-byte JSON body. 4 KiB
	// leaves ~7x headroom over anything the decoder would not reject anyway.
	// Matches the 4 KiB upstream uses, and the existing 4096-byte cap on
	// POST /api/path-inspect (inspectBodyLimit).
	maxDecodeBodyBytes = 4 << 10 // 4 KiB

	// maxBatchObservationsBodyBytes caps POST /api/packets/observations. The
	// biggest request the handler accepts is 200 16-hex content hashes, which
	// serializes to ~3.8 KiB, so 64 KiB leaves ~17x headroom: every request
	// within the 200-hash limit passes, and a client that exceeds that limit
	// still gets the established 400 "too many hashes" rather than a 413.
	// Matches the 64 KiB upstream uses.
	maxBatchObservationsBodyBytes = 64 << 10 // 64 KiB
)

// decodeLimitedJSONBody reads at most limit bytes of r.Body and unmarshals them
// into v. It answers the request itself and returns false when the body is over
// the cap (413, naming the limit) or is not valid JSON (400); the caller returns
// immediately in that case. Both answers use the shared writeError shape
// ({"error": "..."} as application/json, #266), so clients get one error
// contract across every endpoint.
//
// Reading the body to a bounded buffer first — rather than letting a
// json.Decoder stream it — is what makes the cap a byte cap: the verdict
// depends only on how many bytes arrived, never on where in the JSON the
// decoder happened to stop. A body of exactly limit bytes is accepted; limit+1
// is rejected.
func decodeLimitedJSONBody(w http.ResponseWriter, r *http.Request, limit int64, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body too large (max %d bytes)", limit))
			return false
		}
		// A truncated or aborted upload is as unusable as malformed JSON, and
		// json.Decoder answered the same 400 for it before this cap existed.
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}
