#!/usr/bin/env python3
"""Reference vectors for TestDeltaBullington941_MatchesP1812Reference (#941 M2).

A plain-Python transcription of OFCOM's Py1812 (github.com/eeveetza/Py1812,
src/Py1812/P1812.py) for the "delta-Bullington" diffraction loss of ITU-R
P.1812-6 §4.3.4 (dl_delta_bull, eqs. 37-39), with its parts:

  - dl_bull         §4.3.1, eqs. 12-21 (the Bullington loss; the same as
                    bullington_941_vectors.py);
  - dl_se           §4.3.2, eqs. 22-27 (spherical-earth diffraction);
  - dl_se_ft_inner  §4.3.3, eqs. 29-36 (its first-term part);
  - smooth_earth_heights, Attachment 1 §5.6.1-5.6.2, eqs. 85-91d (the
    least-squares smooth surface and its diffraction heights hstd, hsrd).

The model is the one ITU-R P.526-16 §4.5.2 ("complete method") describes;
see the header of internal/dhm/delta.go for what was and was not checked
against the P.526 text.

Choices, mirrored in internal/dhm/delta.go:
  - flag4 = 0: L_bulls is the Bullington loss of the zero-height profile
    (dl_bull on zeros), not P.1812's Attachment 4 closed form;
  - omega = 0 (a land path, eps_r 22, sigma 0.003 S/m) and the vertical
    polarisation (index 1 of Py1812's pair): MeshCore antennas are
    vertical;
  - lambda = c / f with the exact speed of light (dhm.SpeedOfLight), where
    Py1812 uses 0.2998 / f; f in GHz is FREQ_MHZ / 1000;
  - no clutter: g = h.

Units are P.1812's own: distances in km, heights in m, the effective earth
radius ap in km. Standard library only:

    python3 internal/dhm/testdata/delta_bullington_941_vectors.py

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


def dl_bull(d, g, hts, hrs, ap, lam):
    """Py1812 dl_bull: the Bullington loss Lbull (eq. 21)."""
    ce = 1 / ap
    dtot = d[-1] - d[0]
    inner = range(1, len(d) - 1)
    stim = max((g[i] + 500 * ce * d[i] * (dtot - d[i]) - hts) / d[i] for i in inner)    # eq. 14
    s_tr = (hrs - hts) / dtot                                                            # eq. 15
    if stim < s_tr:
        numax = max((g[i] + 500 * ce * d[i] * (dtot - d[i]) - (hts * (dtot - d[i]) + hrs * d[i]) / dtot)
                    * math.sqrt(0.002 * dtot / (lam * d[i] * (dtot - d[i]))) for i in inner)  # eq. 16
        luc = J(numax)                                                                    # eq. 17
    else:
        srim = max((g[i] + 500 * ce * d[i] * (dtot - d[i]) - hrs) / (dtot - d[i]) for i in inner)  # eq. 18
        dbp = (hrs - hts + srim * dtot) / (stim + srim)                                  # eq. 19
        nub = ((hts + stim * dbp - (hts * (dtot - dbp) + hrs * dbp) / dtot)
               * math.sqrt(0.002 * dtot / (lam * dbp * (dtot - dbp))))                   # eq. 20
        luc = J(nub)
    return luc + (1 - math.exp(-luc / 6)) * (10 + 0.02 * dtot)                            # eq. 21


def dl_se_ft_inner(epsr, sigma, d, hte, hre, adft, f):
    """Py1812 dl_se_ft_inner, vertical polarisation only (eqs. 29-36)."""
    kh = 0.036 * (adft * f) ** (-1.0 / 3.0) * ((epsr - 1) ** 2 + (18 * sigma / f) ** 2) ** (-1.0 / 4.0)  # eq. 29a
    k = kh * (epsr ** 2 + (18 * sigma / f) ** 2) ** 0.5                                              # eq. 29b
    beta = (1 + 1.6 * k ** 2 + 0.67 * k ** 4) / (1 + 4.5 * k ** 2 + 1.53 * k ** 4)                  # eq. 30
    x = 21.88 * beta * (f / adft ** 2) ** (1.0 / 3.0) * d                                           # eq. 31
    yt = 0.9575 * beta * (f ** 2 / adft) ** (1.0 / 3.0) * hte                                       # eq. 32a
    yr = 0.9575 * beta * (f ** 2 / adft) ** (1.0 / 3.0) * hre                                       # eq. 32b
    if x >= 1.6:
        fx = 11 + 10 * math.log10(x) - 17.6 * x
    else:
        fx = -20 * math.log10(x) - 5.6488 * x ** 1.425                                             # eq. 33

    def gy(y):
        b = beta * y                                                                             # eq. 35
        if b > 2:
            v = 17.6 * (b - 1.1) ** 0.5 - 5 * math.log10(b - 1.1) - 8
        else:
            v = 20 * math.log10(b + 0.1 * b ** 3) if b > 0 else float("-inf")
        return max(v, 2 + 20 * math.log10(k))                                                    # below eq. 34
    return -fx - gy(yt) - gy(yr)                                                                 # eq. 36


def dl_se(d, hte, hre, ap, f, lam):
    """Py1812 dl_se over land (omega = 0): the spherical-earth loss Ldsph."""
    def ft(adft):
        return dl_se_ft_inner(22, 0.003, d, hte, hre, adft, f)
    dlos = math.sqrt(2.0 * ap) * (math.sqrt(0.001 * hte) + math.sqrt(0.001 * hre))  # eq. 22
    if d >= dlos:
        return ft(ap)
    c = (hte - hre) / (hte + hre)                                                    # eq. 24d
    m = 250 * d * d / (ap * (hte + hre))                                             # eq. 24e
    b = 2 * math.sqrt((m + 1.0) / (3.0 * m)) * math.cos(
        math.pi / 3 + 1.0 / 3.0 * math.acos(3 * c / 2.0 * math.sqrt(3.0 * m / (m + 1.0) ** 3)))  # eq. 24c
    dse1 = d / 2.0 * (1.0 + b)                                                       # eq. 24a
    dse2 = d - dse1                                                                  # eq. 24b
    hse = ((hte - 500 * dse1 * dse1 / ap) * dse2 + (hre - 500 * dse2 * dse2 / ap) * dse1) / d  # eq. 23
    hreq = 17.456 * math.sqrt(dse1 * dse2 * lam / d)                                 # eq. 25
    if hse > hreq:
        return 0.0
    aem = 500 * (d / (math.sqrt(hte) + math.sqrt(hre))) ** 2                         # eq. 26
    return (1 - hse / hreq) * max(ft(aem), 0.0)                                      # eq. 27


def diffraction_heights(d, h, hts, hrs):
    """Py1812 smooth_earth_heights: hstd, hsrd (eqs. 85-91d), no clutter."""
    n = len(d)
    dtot = d[-1]
    v1 = sum((d[i] - d[i - 1]) * (h[i] + h[i - 1]) for i in range(1, n))                       # eq. 85
    v2 = sum((d[i] - d[i - 1]) * (h[i] * (2 * d[i] + d[i - 1]) + h[i - 1] * (d[i] + 2 * d[i - 1]))
             for i in range(1, n))                                                             # eq. 86
    hst = (2 * v1 * dtot - v2) / dtot ** 2                                                     # eq. 87
    hsr = (v2 - v1 * dtot) / dtot ** 2                                                         # eq. 88
    hh = [h[i] - (hts * (dtot - d[i]) + hrs * d[i]) / dtot for i in range(n)]                  # eq. 89d
    hobs = max(hh[1:-1])                                                                       # eq. 89a
    aobt = max(hh[i] / d[i] for i in range(1, n - 1))                                          # eq. 89b
    aobr = max(hh[i] / (dtot - d[i]) for i in range(1, n - 1))                                 # eq. 89c
    if hobs <= 0:
        hstp, hsrp = hst, hsr                                                                  # eqs. 90a-b
    else:
        gt = aobt / (aobt + aobr)                                                              # eq. 90e
        gr = aobr / (aobt + aobr)                                                              # eq. 90f
        hstp, hsrp = hst - hobs * gt, hsr - hobs * gr                                          # eqs. 90c-d
    hstd = h[0] if hstp >= h[0] else hstp                                                      # eqs. 91a-b
    hsrd = h[-1] if hsrp > h[-1] else hsrp                                                     # eqs. 91c-d
    return hstd, hsrd


def dl_delta_bull(d, h, hts, hrs, ap, f, lam):
    """Py1812 dl_delta_bull with flag4 = 0: (Ld, Lbulla, Lbulls, Ldsph)."""
    hstd, hsrd = diffraction_heights(d, h, hts, hrs)
    lbulla = dl_bull(d, h, hts, hrs, ap, lam)
    hts1, hrs1 = hts - hstd, hrs - hsrd                                              # eqs. 37a-b
    lbulls = dl_bull(d, [0.0] * len(d), hts1, hrs1, ap, lam)
    ldsph = dl_se(d[-1] - d[0], hts1, hrs1, ap, f, lam)                              # eq. 38
    return lbulla + max(ldsph - lbulls, 0), lbulla, lbulls, ldsph                    # eq. 39


def p1812_profile(phase, ridge, base, n):
    """p1812Profile in diffraction_941_test.go, extended to n samples."""
    g = []
    for i in range(n):
        v = 20 + 15 * math.sin(i / 17.0) + 8 * math.sin(i / 5.3 + phase) + base
        if ridge and i == 120:
            v += 60
        g.append(v)
    return g


# name, profile ("hilly" phase ridge base | "flat" height), samples, step m,
# tx antenna, rx antenna (m above the ground).
CASES = [
    ("hilly nlos 10/2", ("hilly", 0, False, 0), 201, 30, 10, 2),
    ("hilly ridge", ("hilly", 1, True, 7), 201, 30, 10, 2),
    ("hilly los 40/22", ("hilly", 3, False, 0), 201, 30, 40, 22),
    ("hilly los clear", ("hilly", 0, False, 0), 201, 30, 120, 100),
    ("hilly 30 km", ("hilly", 2, False, 14), 1001, 30, 10, 2),
    ("flat 1 km 10/10", ("flat", 5), 34, 30, 10, 10),
    ("flat 5 km los", ("flat", 0), 168, 30, 10, 2),
    ("flat 15 km los", ("flat", 0), 501, 30, 10, 2),
    ("flat 25 km", ("flat", 0), 834, 30, 10, 2),
    ("flat 40 km", ("flat", 12), 1334, 30, 10, 2),
    ("flat 30 km 30/5", ("flat", 3), 1001, 30, 30, 5),
]


def main():
    lam = SPEED_OF_LIGHT / (FREQ_MHZ * 1e6)
    f = FREQ_MHZ / 1000
    ap = K_FACTOR * EARTH_RADIUS_KM
    for name, prof, n, step, ta, ra in CASES:
        if prof[0] == "hilly":
            h = p1812_profile(prof[1], prof[2], prof[3], n)
        else:
            h = [float(prof[1])] * n
        d = [i * step / 1000 for i in range(n)]
        ld, lbulla, lbulls, ldsph = dl_delta_bull(d, h, h[0] + ta, h[-1] + ra, ap, f, lam)
        print('{"%s", %d, %d, %d, %d, %r, %r, %r, %r},' % (name, n, step, ta, ra, lbulla, lbulls, ldsph, ld))


if __name__ == "__main__":
    main()
