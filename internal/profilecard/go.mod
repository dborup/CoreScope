module github.com/meshcore-analyzer/profilecard

go 1.22

require (
	// The shared card primitives (#811 M1): fonts, canvas, colours, PNG.
	github.com/meshcore-analyzer/cardkit v0.0.0
	// The fill credit's parts (#738); stdlib-only beyond internal/dhm.
	github.com/meshcore-analyzer/copdem v0.0.0
	github.com/meshcore-analyzer/dhm v0.0.0
	// Pinned. v0.23.0 keeps the go directive at 1.22, the server's: v0.27.0
	// raises it to 1.23 and v0.48.0 to 1.26 (#727 plan §2.1). Since #811 M1
	// only internal/cardkit imports x/image and x/text; both stay pinned
	// here too, at cardkit's versions, so this module cannot drift.
	golang.org/x/image v0.23.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)

require golang.org/x/sync v0.10.0 // indirect

replace github.com/meshcore-analyzer/dhm => ../dhm

replace github.com/meshcore-analyzer/copdem => ../copdem

replace github.com/meshcore-analyzer/cardkit => ../cardkit
