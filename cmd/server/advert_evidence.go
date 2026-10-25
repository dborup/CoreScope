package main

import "github.com/meshcore-analyzer/packetpath"

// advertEvidence is StoreObs.advertEv: what an observation's own frame says
// about an ADVERT's originator (#488).
type advertEvidence struct {
	// code is 1 + the packetpath.AdvertKind, or 0 when no frame was stored.
	code uint8
	// hops is the relay count of an AdvertRelayed frame: at most 63, the
	// largest count a path byte holds, so it fits a byte (review N2 on
	// #609: the fewest-hops fallback needs it).
	hops uint8
}

// advertEvidenceFromRawHex classifies an observation's frame for
// StoreObs.advertEv. The frame itself is not kept (#881), so this is
// classified once, on load.
func advertEvidenceFromRawHex(rawHex string) advertEvidence {
	if rawHex == "" {
		return advertEvidence{}
	}
	h := packetpath.ClassifyAdvertFrame(rawHex)
	return advertEvidence{code: uint8(h.Kind) + 1, hops: uint8(h.Hops)}
}

// advertHearing classifies one ADVERT observation: by its own frame when one
// was stored, else (a legacy row) by its path_json, unverified.
func (o *StoreObs) advertHearing() packetpath.AdvertHearing {
	if o.advertEv.code != 0 {
		return packetpath.AdvertHearing{Kind: packetpath.AdvertKind(o.advertEv.code - 1), Hops: int(o.advertEv.hops), Verified: true}
	}
	return packetpath.ClassifyAdvertPathJSON(o.PathJSON)
}
