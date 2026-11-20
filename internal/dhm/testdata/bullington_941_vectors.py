#!/usr/bin/env python3
"""Reference vectors for TestBullington941_MatchesP1812Reference (#941).

A plain-Python transcription of OFCOM's Py1812 dl_bull: the Bullington
part of ITU-R P.1812-6 §4.3.1, eqs. 12-21 (the same equations as P.526
§4.5.1), in P.1812's own units: distances in km, heights in m, the
effective earth curvature Ce in 1/km. It does not use the A·B reduction
of internal/dhm/diffraction.go, so it checks that reduction independently.

The profiles are p1812Profile in diffraction_941_test.go: 201 samples
30 m apart (6 km), in float64. Standard library only:

    python3 internal/dhm/testdata/bullington_941_vectors.py

prints one Go table row per case, as pasted into the test.
"""
import math

SPEED_OF_LIGHT = 299792458.0  # m/s, dhm.SpeedOfLight
FREQ_MHZ = 869.618            # dhm.FirmwareFreqMHz
K_FACTOR = 4.0 / 3            # dhm.DefaultKFactor
EARTH_RADIUS_KM = 6371.0      # dhm.EarthRadiusM / 1000


def J(nu):
    """Knife-edge loss, eq. 12; 0 at and below nu = -0.78."""
    if nu <= -0.78:
        return 0.0
    return 6.9 + 20 * math.log10(math.sqrt((nu - 0.1) ** 2 + 1) + nu - 0.1)


def dl_bull(d, h, hts, hrs, ce, lam):
    """Bullington loss of a path: d in km, h and the antenna heights hts,
    hrs (above the datum) in m, ce in 1/km, lam in m. Returns
    (los, nu, Luc, Lbull)."""
    dtot = d[-1]
    inner = range(1, len(d) - 1)
    bulge = [h[i] + 500 * ce * d[i] * (dtot - d[i]) for i in range(len(d))]
    stim = max((bulge[i] - hts) / d[i] for i in inner)                     # eq. 14
    s_tr = (hrs - hts) / dtot                                              # eq. 15
    if stim < s_tr:  # line of sight
        nu = max((bulge[i] - (hts * (dtot - d[i]) + hrs * d[i]) / dtot)
                 * math.sqrt(0.002 * dtot / (lam * d[i] * (dtot - d[i])))
                 for i in inner)                                           # eq. 16
        los = True
    else:
        srim = max((bulge[i] - hrs) / (dtot - d[i]) for i in inner)        # eq. 18
        dbp = (hrs - hts + srim * dtot) / (stim + srim)                    # eq. 19
        nu = ((hts + stim * dbp - (hts * (dtot - dbp) + hrs * dbp) / dtot)
              * math.sqrt(0.002 * dtot / (lam * dbp * (dtot - dbp))))      # eq. 20
        los = False
    luc = J(nu)                                                            # eqs. 17, 20
    lb = luc + (1 - math.exp(-luc / 6)) * (10 + 0.02 * dtot)               # eq. 21
    return los, nu, luc, lb


def profile(phase, ridge, base):
    """p1812Profile in diffraction_941_test.go."""
    g = []
    for i in range(201):
        v = 20 + 15 * math.sin(i / 17.0) + 8 * math.sin(i / 5.3 + phase) + base
        if ridge and i == 120:
            v += 60
        g.append(v)
    return g


# name, phase, ridge, base, tx antenna, rx antenna (m above the ground).
CASES = [
    ("nlos 10/2", 0, False, 0, 10, 2),
    ("nlos ridge", 1, True, 7, 10, 2),
    ("nlos 2/2", 2, False, 14, 2, 2),
    ("nlos 30/5", 3, False, 21, 30, 5),
    ("los clear", 0, False, 0, 120, 100),
    ("los 34/30", 1, False, 0, 34, 30),
    ("los 31/28", 2, False, 0, 31, 28),
    ("los 40/22", 3, False, 0, 40, 22),
]


def main():
    lam = SPEED_OF_LIGHT / (FREQ_MHZ * 1e6)
    ce = 1 / (K_FACTOR * EARTH_RADIUS_KM)
    d = [i * 0.03 for i in range(201)]
    for name, phase, ridge, base, ta, ra in CASES:
        h = profile(phase, ridge, base)
        los, nu, luc, lb = dl_bull(d, h, h[0] + ta, h[-1] + ra, ce, lam)
        print('{"%s", %d, %s, %d, %d, %d, %s, %r, %r, %r},' % (
            name, phase, str(ridge).lower(), base, ta, ra, str(los).lower(), nu, luc, lb))


if __name__ == "__main__":
    main()
