module github.com/meshcore-analyzer/nodecard

go 1.22

require (
	// The shared card primitives (#811 M1): fonts, canvas, colours, PNG.
	github.com/meshcore-analyzer/cardkit v0.0.0
	// Pinned. v0.23.0 keeps the go directive at 1.22, the server's (#727
	// plan §2.1). Only internal/cardkit imports x/image and x/text; both
	// stay pinned here too, at cardkit's versions, so this module cannot
	// drift.
	golang.org/x/image v0.23.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)

replace github.com/meshcore-analyzer/cardkit => ../cardkit
