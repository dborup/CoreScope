package buildings

import (
	"strings"

	"github.com/meshcore-analyzer/dhm"
)

// The credit of the footprints (#730 research §4, coordinator decision
// 4). INSPIRE – Bygninger is CC BY 4.0, like DHM, and its lineage says
// "Data er fra GeoDanmark"; GeoDanmark asks to be credited as @geodanmark
// with a link to its terms. The handle is text only, never a link to an
// account.
const (
	// DatasetName is the dataset the footprints come from; DatasetURL is
	// its page on Dataforsyningen.
	DatasetName = "INSPIRE – Bygninger"
	DatasetURL  = "https://dataforsyningen.dk/data/2677"
	// Source is the dataset's own source, credited by its Handle, with
	// SourceTermsURL as the link GeoDanmark's terms ask for.
	Source         = "GeoDanmark"
	SourceHandle   = "@geodanmark"
	SourceTermsURL = "https://www.geodanmark.dk/home/vejledninger/vilkaar-for-data-anvendelse/"
)

// AttributionDerived is the credit of a terrain profile whose surface was
// classified with the footprints: dhm.AttributionDerived, naming the
// footprints and their source after DHM. Every other word is DHM's own
// credit, so the two cannot drift apart:
//
//	Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel og INSPIRE – Bygninger (GeoDanmark, @geodanmark), CC BY 4.0 — data bearbejdet
var AttributionDerived = strings.Replace(dhm.AttributionDerived, dhm.DatasetName,
	dhm.DatasetName+" og "+DatasetName+" ("+Source+", "+SourceHandle+")", 1)
