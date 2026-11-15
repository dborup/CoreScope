module github.com/corescope/smoke

go 1.22

// #803: the manual live smoke check imports each upstream client's own
// request builder, so it checks exactly the shape the server sends.
require (
	github.com/meshcore-analyzer/buildings v0.0.0
	github.com/meshcore-analyzer/copdem v0.0.0
	github.com/meshcore-analyzer/dhm v0.0.0
	github.com/meshcore-analyzer/heritage v0.0.0
	github.com/meshcore-analyzer/mastedb v0.0.0
	github.com/meshcore-analyzer/skraafoto v0.0.0
)

require golang.org/x/sync v0.10.0 // indirect

replace github.com/meshcore-analyzer/buildings => ../../internal/buildings

replace github.com/meshcore-analyzer/copdem => ../../internal/copdem

replace github.com/meshcore-analyzer/dhm => ../../internal/dhm

replace github.com/meshcore-analyzer/heritage => ../../internal/heritage

replace github.com/meshcore-analyzer/mastedb => ../../internal/mastedb

replace github.com/meshcore-analyzer/skraafoto => ../../internal/skraafoto
