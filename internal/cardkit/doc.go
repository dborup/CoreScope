// Package cardkit holds the drawing primitives every link-preview card
// shares (#811 M1): the embedded Go fonts with deterministic glyph
// rasterising and name sanitising (text.go), an integer-coverage canvas
// (raster.go), theme.json colour parsing and the browser's light/dark
// merge (color.go), and the PNG encoder (png.go).
//
// A card package (internal/profilecard for terrain profiles, #727) draws
// its own layout with these and keeps its own palette type. Everything
// here is a pure function of its arguments: no I/O, no package state, and
// the same pixels for the same input on every architecture, which is what
// lets the cards' golden images be compared byte for byte.
//
// cardkit knows nothing about terrain, DHM or any data source; deps_test
// pins that.
package cardkit
