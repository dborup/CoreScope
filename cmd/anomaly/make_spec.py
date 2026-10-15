# make_spec.py - builds the #474 round-3 injection spec deterministically.
# python3 make_spec.py <V.start RFC3339> <keys.json> > spec.json   (stdlib only)
#
# The panel is paired. Each shape appears twice at the same time, on two
# different stream keys:
#   shared   - the key of real background traffic, taken from the keys ranking
#              by row count (rank 1..9 in order). This is the arm that
#              measures detection inside real traffic.
#   isolated - the same channel byte with the 3-byte first hop fdff0i, a key
#              no real relayed line carries. This is the round-1 control.
# The two twins never interact: they are different keys.
#
# Sizes come from the key's own observed (wire_bytes, payload_bytes)
# distribution, passed through verbatim as size_mix. Round 2 instead held
# wire_bytes at the key's median while drawing the payload from
# median +/- (p90 - p10) / 2, which has two faults: a real GRP_TXT payload
# comes in 16-byte cipher blocks while wire - payload is only header plus
# path, so the drawn payload routinely exceeded wire - 2 and inject aborted
# on the event validator (#478 review G1); and even where it fitted, the
# drawn sizes were ones the key never carried, so an injected line was
# distinguishable on size alone (G2). Drawing whole observed pairs fixes
# both: every pair came from a real line, so it is valid by construction and
# indistinguishable by size.
#
# Jitter comes from #130's measurements of a real 5-minute bot over 85.2 h
# (|gap - 300 s| median 3 s, p95 9 s), not from the rule's tolerance: a gap
# offset is the difference of two per-pulse offsets, so sd(gap) = sqrt(2) *
# sd(pulse), and p95 9 s gives sd(gap) ~ 9/1.96 ~ 4.6 s and sd(pulse) ~ 3.2 s.
# So every bot gets jitter_sd 3 s and jitter_max 9 s, whatever its period and
# whatever the rule's tolerance is.
import json, sys
from datetime import datetime, timedelta, timezone

V0 = datetime.strptime(sys.argv[1], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
RANKING = json.load(open(sys.argv[2]))


def iso(t):
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


# Panel: (period seconds, shape). Shapes:
#   clean = jitter only
#   noisy = jitter + 5 % missed pulses + 30 % multi-part pulses (#130 shape)
#   sept  = noisy plus the one 35-minute outage of the September bot
PANEL = [(90, "clean"), (120, "clean"), (300, "clean"), (600, "clean"), (900, "clean"),
         (90, "noisy"), (300, "noisy"), (900, "noisy"), (300, "sept")]
HOURS_EACH, HOURS_APART = 12, 18
JITTER_SD, JITTER_MAX = 3, 9          # #130, measured
SEED = 474202610170

keys = RANKING["top"]
if len(keys) < len(PANEL):
    sys.exit("keys ranking has %d keys, need %d" % (len(keys), len(PANEL)))

bots = []
for i, (p, shape) in enumerate(PANEL):
    k = keys[i]                        # rank i+1, by row count alone
    start = V0 + timedelta(hours=1 + HOURS_APART * i)
    stop = start + timedelta(hours=HOURS_EACH)
    mpd = min(55, p // 4)
    noisy = shape in ("noisy", "sept")
    holes = [{"start": iso(start + timedelta(hours=6)), "duration": "35m"}] if shape == "sept" else []
    # The key's observed size distribution, verbatim. inject draws one whole
    # (wire, payload) pair per event, weighted by count, so the injected
    # sizes are exactly the sizes the key carries.
    size_mix = k["size_mix"]
    if not size_mix:
        sys.exit("rank %d has no observed size pair" % (i + 1))
    for arm, hop in (("shared", k["first_hop"]), ("iso", "3:fdff%02x" % i)):
        bots.append({
            "name": "b%d-%ds-%s-%s" % (i, p, shape, arm),
            "channel": k["channel"], "payload_type": RANKING["payload_type"],
            "route_class": k["route_class"], "route_kind": k["route_kind"],
            "first_hop": hop, "full_route": k["full_route"] if arm == "shared" else hop,
            "size_mix": size_mix,
            "start": iso(start), "stop": iso(stop), "period": "%ds" % p,
            "jitter_sd": "%ds" % JITTER_SD, "jitter_max": "%ds" % JITTER_MAX,
            "missed_fraction": 0.05 if noisy else 0.0,
            "multipart_fraction": 0.3 if noisy else 0.0,
            "multipart_max_extra": 2, "multipart_max_delay": "%ds" % mpd,
            "holes": holes,
        })

print(json.dumps({"seed": SEED, "final_after": "30s",
                  "labelling": {"pulse_merge_gap": "60s", "min_period": "1m", "max_period": "1h",
                                "min_pulses": 8, "max_harmonic": 3,
                                "tolerance_abs": "30s", "tolerance_rel": 0.1},
                  "bots": bots}, indent=1))
