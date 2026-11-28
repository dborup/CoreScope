package main

// The estimated-positions operator policy lives on *Server, never on the
// shared *DB handle, so every production path lookup passes the effective
// policy explicitly (see getPacketPath and getPacketPathsBulk).
//
// These always-estimating wrappers exist only so the pre-#315 path
// fixtures keep reading the way they did. They live in a _test.go file on
// purpose: a future production caller cannot reach them -- the server
// binary would not compile -- so no caller can hard-wire "estimates on"
// and silently bypass the operator's policy.
func (db *DB) testPacketPath(hash string, maxEdgeKm float64) (*PacketPathResponse, error) {
	return db.getPacketPath(hash, maxEdgeKm, true)
}

func (db *DB) testPacketPathsBulk(hashes []string, maxEdgeKm float64) (map[string]*PacketPathResponse, error) {
	return db.getPacketPathsBulk(hashes, maxEdgeKm, true)
}
