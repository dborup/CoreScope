package main

import "github.com/meshcore-analyzer/packetpath"

// advertEvidenceFromRawHex encodes what an observation's own frame says about
// an ADVERT's originator for StoreObs.advertEv (#488): 1 + the
// packetpath.ClassifyAdvertFrame kind, or 0 when no frame was stored. The
// frame itself is not kept (#881), so this is classified once, on load.
func advertEvidenceFromRawHex(rawHex string) uint8 {
	if rawHex == "" {
		return 0
	}
	return uint8(packetpath.ClassifyAdvertFrame(rawHex).Kind) + 1
}

// advertHearing classifies one ADVERT observation: by its own frame when one
// was stored, else (a legacy row) by its path_json, unverified.
func (o *StoreObs) advertHearing() packetpath.AdvertHearing {
	if o.advertEv != 0 {
		return packetpath.AdvertHearing{Kind: packetpath.AdvertKind(o.advertEv - 1), Verified: true}
	}
	return packetpath.ClassifyAdvertPathJSON(o.PathJSON)
}
