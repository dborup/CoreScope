package profilecard

// The renderer's input is the body of GET /api/terrain/profile. These
// types mirror cmd/server's TerrainProfileResponse field for field and tag
// for tag, under the same names, so the server can alias them once it
// imports this package (#727 M3) instead of converting. cmd/server is
// package main and cannot be imported from here, so the mirror is pinned
// from both sides by testdata/response_shape.txt: shape_test.go here and
// cmd/server/profilecard_shape_test.go there both compare their own
// reflection of the types against that one file.

// TerrainProfileResponse is one terrain profile, as the API serves it.
type TerrainProfileResponse struct {
	A           TerrainEndpoint `json:"a"`
	B           TerrainEndpoint `json:"b"`
	DistanceM   float64         `json:"distanceM"`
	StepM       float64         `json:"stepM"`
	SampleCount int             `json:"sampleCount"` // computed; samples holds at most 1024 of them
	Model       string          `json:"model"`       // "dtm" or "both"
	Coverage    TerrainCoverage `json:"coverage"`
	Samples     []TerrainSample `json:"samples"`
	Physics     TerrainPhysics  `json:"physics"`
	Verdict     TerrainVerdict  `json:"verdict"`
	Attribution string          `json:"attribution"`
}

// TerrainEndpoint is one end of the path, snapped to 5 decimals.
type TerrainEndpoint struct {
	Lat           float64  `json:"lat"`
	Lon           float64  `json:"lon"`
	Kind          string   `json:"kind"` // "node" or "point"
	Role          string   `json:"role,omitempty"`
	GroundM       *float64 `json:"groundM"` // DTM; null outside coverage
	AntennaM      float64  `json:"antennaM"`
	AntennaSource string   `json:"antennaSource"`
	Assumed       bool     `json:"assumed"` // the height is a default, not a known value
}

// TerrainCoverage is how much of the path the deciding model covers.
type TerrainCoverage struct {
	State     string   `json:"state"` // full, partial, none
	Pct       float64  `json:"pct"`
	FirstGapM *float64 `json:"firstGapM"`
}

// TerrainSample is one point along the path; nil where there is no data.
type TerrainSample struct {
	D   float64  `json:"d"`
	DTM *float64 `json:"dtm"`
	DSM *float64 `json:"dsm,omitempty"`
}

// TerrainPhysics are the radio parameters the verdict was computed with.
type TerrainPhysics struct {
	FreqMHz    float64 `json:"freqMHz"`
	FreqSource string  `json:"freqSource"`
	LambdaM    float64 `json:"lambdaM"`
	K          float64 `json:"k"`
	KSource    string  `json:"kSource"`
	F1MaxM     float64 `json:"f1MaxM"`
}

// TerrainVerdict is the terrain (DTM) verdict, plus the surface (DSM) one
// for information with model=both.
type TerrainVerdict struct {
	State     string  `json:"state"` // clear, fresnel100, fresnel60, blocked, unknown
	Model     string  `json:"model"`
	Criterion float64 `json:"criterion"`
	TerrainClearance
	Surface *TerrainSurfaceInfo `json:"surface,omitempty"`
}

// TerrainClearance is the worst point of an assessment; nil without one.
// WorstF1Fraction is also nil when the worst point is a surface above an
// antenna (#745).
type TerrainClearance struct {
	WorstClearanceM *float64 `json:"worstClearanceM"`
	WorstAtM        *float64 `json:"worstAtM"`
	WorstF1Fraction *float64 `json:"worstF1Fraction"`
}

// TerrainSurfaceInfo is the surface model's assessment. Reason is
// "antennaBelowSurface" when the surface is above the antenna at an
// endpoint (#745); Endpoint is then "A", "B" or "both".
type TerrainSurfaceInfo struct {
	State string `json:"state"`
	TerrainClearance
	Reason   string `json:"reason,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}
