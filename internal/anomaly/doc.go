// Package anomaly is an EXPERIMENTAL, OFFLINE library for
// encryption-independent traffic anomaly detection on MeshCore transmissions.
//
// # Status
//
//   - Experimental and offline only. Nothing in CoreScope calls this package:
//     no ingestor, server, API, UI, database, config, webhook or notification
//     wiring exists. Its only callers are its own tests and benchmarks.
//   - There are NO default thresholds. Every limit and threshold must be set
//     explicitly in Config; New rejects the zero value and rejects a config
//     that enables no rule. Values used in tests are named experimental
//     fixtures, not recommendations.
//   - Phase 1 of the study (a 30-day staging snapshot) validated only three
//     incidents. No rule here is validated. A blind validation on a fresh
//     snapshot starting no earlier than 2026-10-08 is required before any
//     threshold is proposed for production.
//
// # What "encryption-independent" means
//
// Detection uses only on-wire, key-free facts supplied by the caller:
// payload type, an opaque channel hash, route class, the first relay hop
// with its byte width, the full route (as separate evidence), frame sizes
// and data timestamps. It does not need to decrypt anything and it does not
// depend on names such as "enc_XX" or on whether the caller's instance could
// decrypt a channel (DecryptStatus is metadata only).
//
// Encryption-independent does not mean identity-independent, and it is not
// attribution:
//
//   - A stream key is an observed route group: channel hash + route class +
//     first hop. It is not a sender. Mobile senders spread over many route
//     groups; one busy relay carries many senders.
//   - The first hop is the first relay after the originator as seen on the
//     wire. 1-byte hops are globally ambiguous (many nodes share a prefix);
//     2- and 3-byte hops much less so. A 1-byte hop and the prefix of a wider
//     hop are different values.
//   - The observer is never assumed to be the sender or the first relay.
//     No-path, direct, mixed and unknown routes are a separate low-confidence
//     class and never form a route group; Event.Validate rejects a relay or
//     no-path kind without a flood (or mixed) route class.
//   - A global (payload-type) candidate is never attributed to a stream,
//     channel or route.
//
// # Data contract
//
// The Detector consumes FINAL Events, one per logical transmission, and
// counts each Event.ID at most once within Limits.DedupHorizon (bounded by
// Limits.MaxDedupIDs; early evictions are counted and flagged). Observations
// are evidence of receivers and routes only and never multiply a rate.
// Callers either finish normalization themselves or use Normalizer, whose
// finality policy is: an event is final at first arrival + SettleWindow,
// and observations arriving after that are rejected as late, so a replay
// never uses route evidence that arrived after the decision. When the
// Normalizer's own caps cut a transmission's evidence, the Event is marked
// Truncated and candidates nearby carry CoverageInputTruncated.
//
// Candidates carry DecidedAt from a monotone decision clock: the latest
// FinalAt of every event offered so far, and within Advance the earliest
// time an Advance could have released the item (data time +
// ReorderDelay; Flush likewise). DecidedAt is therefore >= the FinalAt of
// every event behind a decision (not only the trigger), a time-driven end
// is never decided before the transitions that opened its episode nor,
// except after a forced release on reorder-buffer overflow (flagged
// CoverageReorderForced), before its deadline + ReorderDelay, and events
// offered in finalization order give the decision times of a live
// detector.
//
// Upstream deduplication by content hash (as in CoreScope's database) merges
// byte-identical re-broadcasts into one transmission before this library
// sees them; such repeats cannot be counted here.
//
// # Time
//
// Only data time is used. Valid times lie after the Unix epoch and before
// 2200. Events must arrive in data-time order or within
// Limits.ReorderDelay of it; older events are rejected as late (counted),
// never silently misplaced. With ReorderDelay > 0, events with equal times
// are processed in ID order and one that arrives after its tie was
// processed and would sort first is late too; with ReorderDelay 0 equal
// times keep arrival order. Deadlines (quiet ends, cooldowns) are
// processed in data-time order together with events, so the output depends
// only on the input and the config, not on when Advance or Flush were
// called; the one exception is DecidedAt, which records when the call
// sequence made each decision possible.
//
// # Building blocks implemented
//
//   - RateRule: sliding windows (t-W, t] at stream, channel, route-group or
//     global scope, any width from 1 s to 24 h.
//   - NewStreamRule: causal new-stream volume with left censoring (the
//     censor window must cover QuietPeriod) and reactivation after silence.
//   - PeriodicRule: causal periodicity with explicit burst, jitter, missing
//     pulse and harmonic handling, recovery after stray pulses, and a
//     local-rate Poisson null with a union bound over the period search.
//   - A per-episode state machine normal -> suspicious -> active -> ended
//     with confirmation time, quiet end, cooldown, stable EventIDs and
//     reason codes.
//
// # Not implemented
//
// Deliberately NOT implemented: hash-rotation, baseline-deviation and
// large-packet rules. Phase 1 did not validate them and they would widen
// this change; route-group and global rate windows cover the multi-channel
// storm seen in phase 1.
//
// # Memory and coverage
//
// Memory is bounded by Limits (keys per scope, dedup IDs, window entries per
// key, reorder buffer, candidate buffer) and by the fixed pulse ring per
// periodic rule. When a limit cuts evidence, Stats counts it, the Result
// that saw it has CoverageReduced set and affected candidates carry
// CoverageFlags; time-driven and eviction transitions keep the traffic
// label and coverage flags of the episode they close.
//
// # Concurrency
//
// A Detector or Normalizer is not safe for concurrent use.
// Separate instances share no state.
package anomaly
