module github.com/meshcore-analyzer/profilecard

go 1.22

require (
	// The fill credit's parts (#738); stdlib-only beyond internal/dhm.
	github.com/meshcore-analyzer/copdem v0.0.0
	github.com/meshcore-analyzer/dhm v0.0.0
	// Pinned. v0.23.0 keeps the go directive at 1.22, the server's: v0.27.0
	// raises it to 1.23 and v0.48.0 to 1.26 (#727 plan §2.1). Fonts and
	// glyph outlines (font/sfnt, font/gofont) and the fixed-point
	// rasteriser (vector) are all the card needs from it.
	golang.org/x/image v0.23.0
	// Already in the graph through x/image (font/sfnt); used directly for
	// unicode/norm (NFC names, #755 review).
	golang.org/x/text v0.21.0
)

require golang.org/x/sync v0.10.0 // indirect

replace github.com/meshcore-analyzer/dhm => ../dhm

replace github.com/meshcore-analyzer/copdem => ../copdem
