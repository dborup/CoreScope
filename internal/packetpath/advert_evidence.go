package packetpath

// Stub for the #488 M1 red commit: the types the tests need, no behaviour.

// AdvertKind is what one ADVERT observation says about its originator.
type AdvertKind uint8

const (
	AdvertUnknown AdvertKind = iota
	AdvertDirect
	AdvertRelayed
	AdvertShared
)

// AdvertHearing classifies one ADVERT observation.
type AdvertHearing struct {
	Kind     AdvertKind
	Hops     int
	Verified bool
}

func ClassifyAdvert(obsRawHex, pathJSON string) AdvertHearing { return AdvertHearing{} }
func ClassifyAdvertFrame(rawHex string) AdvertHearing         { return AdvertHearing{} }
func ClassifyAdvertPathJSON(pathJSON string) AdvertHearing    { return AdvertHearing{} }
