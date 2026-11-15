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

© DLR e.V. 2010-2014 and © Airbus Defence and Space GmbH 2014-2018 provided
under COPERNICUS by the European Union and ESA; all rights reserved.

The organisations in charge of the Copernicus programme by law or by
delegation do not incur any liability for any use of the Copernicus
WorldDEM-30.
