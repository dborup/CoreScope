# internal/dhm testdata

## glo30-n55e012-ifds.bin

The first 1224 bytes of the Copernicus DEM GLO-30 tile
`Copernicus_DSM_COG_10_N55_00_E012_00_DEM.tif` (15 746 303 bytes,
`Last-Modified: Mon, 09 May 2022`) from the public AWS Open Data bucket
`copernicus-dem-30m`. It was fetched once on 2026-11-14 with one ranged
`GET` (`Range: bytes=0-16383`) and cut to 1224 bytes. No test fetches it.

Those bytes are the TIFF header, GDAL's structural-metadata block, the
four IFDs (full resolution and three overviews) and their out-of-line
values, including the block offset and byte-count tables. They end exactly
where the first block's data begins, so the file holds **no elevation
values**. `TestParseTiledGeoTIFFRealGLO30Header` pins the layout the
foreign fill (#738) depends on against it.

sha256 `2281b1a32b2b02b4ff021480c13e4dd26782ddbf75e3acff95deb7b1d2dbb5cb`.

## dk_land_hist_860.txt

The share of Danish land per whole metre of height (0-200 m; 0 holds every
height at or below 0 m, 200 every height from 200 m), which the #860
colour-by-elevation stops are chosen from and
`TestElevation860_BandsShareTheLand` checks them against. It is a
statistic, not elevation data: 201 numbers.

`TestElevation860_MeasureLandDistribution` wrote it
(`DHM_860_GLO90_DIR=<dir> DHM_860_WRITE_HIST=1`) from the 20 Copernicus
DEM GLO-90 tiles over Danish land,
`Copernicus_DSM_COG_30_N{54..57}_00_E{008..015}_00_DEM.tif`
(`Last-Modified: Mon, 09 May 2022`), fetched once on 2026-11-16 from the
public AWS Open Data bucket `copernicus-dem-90m`. A pixel counts as land
where `Covers` holds (the DHM extent minus foreign land, #692) and its
height is not exactly 0 (GLO's sea), weighted by its area: 42,650 km² in
all, against Denmark's 42,951 km². No test fetches anything. GLO-90 is a
90 m surface model (trees and buildings included), so forests read some
metres higher than in DHM/Terræn.

## bullington_941_vectors.py

The generator of the reference vectors in
`TestBullington941_MatchesP1812Reference` (#941): a plain-Python
transcription of OFCOM's Py1812 `dl_bull` (ITU-R P.1812-6 §4.3.1,
eqs. 12-21) on the test's synthetic profiles. Standard library only;
`python3 internal/dhm/testdata/bullington_941_vectors.py` prints the
test's table rows. No test runs it.

## delta_bullington_941_vectors.py

The generator of the reference vectors in
`TestDeltaBullington941_MatchesP1812Reference` (#941 M2): a plain-Python
transcription of OFCOM's Py1812 `dl_delta_bull`, `dl_bull`, `dl_se`,
`dl_se_ft_inner` and the diffraction heights of `smooth_earth_heights`
(ITU-R P.1812-6 §4.3.1-4.3.4 and Attachment 1 §5.6.1-5.6.2, the method of
ITU-R P.526-16 §4.5.2) on hilly and flat profiles from 1 to 40 km. Land,
vertical polarisation, the exact speed of light, no clutter. Standard
library only; `python3 internal/dhm/testdata/delta_bullington_941_vectors.py`
prints the test's table rows. No test runs it.

## Copernicus data

`glo30-n55e012-ifds.bin` and `dk_land_hist_860.txt`: © DLR e.V. 2010-2014 and © Airbus Defence and Space GmbH 2014-2018 provided
under COPERNICUS by the European Union and ESA; all rights reserved.

The organisations in charge of the Copernicus programme by law or by
delegation do not incur any liability for any use of the Copernicus
WorldDEM-30 and WorldDEM-90.
