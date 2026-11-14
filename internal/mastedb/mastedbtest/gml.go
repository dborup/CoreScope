// Package mastedbtest builds synthetic Mastedatabasen GetFeature responses
// for tests outside internal/mastedb (the cmd/server route, #724 M2), so no
// test ever needs the real WFS. It does not import mastedb, so mastedb's own
// tests can use it too.
package mastedbtest

import (
	"fmt"
	"strings"
)

// cellSizeM is mastedb.CellSizeM; TestGMLRowsInsideCell in mastedb pins
// that the two agree.
const cellSizeM = 2000

const header = `<?xml version="1.0" encoding="UTF-8"?><wfs:FeatureCollection ` +
	`xmlns:wfs="http://www.opengis.net/wfs/2.0" xmlns:mastedatabasen="www.mastedatabasen.dk" ` +
	`xmlns:gml="http://www.opengis.net/gml/3.2" numberMatched="%s" numberReturned="%d">`

// GML is a synthetic response for cell (cellE, cellN) with n rows at sites
// of six rows each, the density observed in the #724 samples; every row
// lies inside the cell for n up to 10560 (1760 sites). matched is the
// numberMatched attribute verbatim. Every site has a 700, 800 and 900 MHz
// entry, so every site is interference-relevant.
func GML(cellE, cellN, n int, matched string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, header, matched, n)
	bands := []int{700, 800, 900, 1800, 2100, 3500}
	techs := []string{"LTE", "LTE", "GSM", "LTE", "5G-NR", "5G-NR"}
	for i := 0; i < n; i++ {
		site := i / 6
		e := cellE*cellSizeM + 10 + (site%40)*45
		north := cellN*cellSizeM + 10 + (site/40)*45
		fmt.Fprintf(&b, `<wfs:member><mastedatabasen:MasteIBrug gml:id="MasteIBrug.fid-gen-%d">`+
			`<mastedatabasen:MasteID>%d</mastedatabasen:MasteID>`+
			`<mastedatabasen:Unik_Station_navn>GEN-%d</mastedatabasen:Unik_Station_navn>`+
			`<mastedatabasen:Idriftsaettelsesdato>2024-07-12T22:00:00Z</mastedatabasen:Idriftsaettelsesdato>`+
			`<mastedatabasen:Vejnavn>Genvej</mastedatabasen:Vejnavn>`+
			`<mastedatabasen:Husnummer>%d</mastedatabasen:Husnummer>`+
			`<mastedatabasen:Postnr>8800</mastedatabasen:Postnr>`+
			`<mastedatabasen:By>Testby</mastedatabasen:By>`+
			`<mastedatabasen:TjenesteArtID>2</mastedatabasen:TjenesteArtID>`+
			`<mastedatabasen:TeknologiID>42</mastedatabasen:TeknologiID>`+
			`<mastedatabasen:Objekt_type/>`+
			`<mastedatabasen:UTM_E>%d</mastedatabasen:UTM_E>`+
			`<mastedatabasen:UTM_N>%d</mastedatabasen:UTM_N>`+
			`<mastedatabasen:TjenesteArt_navn>Mobiltelefoni</mastedatabasen:TjenesteArt_navn>`+
			`<mastedatabasen:Teknologi_navn>%s</mastedatabasen:Teknologi_navn>`+
			`<mastedatabasen:Frekvensbaand>%d</mastedatabasen:Frekvensbaand>`+
			`</mastedatabasen:MasteIBrug></wfs:member>`,
			i, 2000000+i, site, site, e, north, techs[i%6], bands[i%6])
	}
	b.WriteString(`</wfs:FeatureCollection>`)
	return []byte(b.String())
}
