package main

// #727 M1 — GET /p?profile=<A>,<B>[&model=][&ha=][&hb=][&k=][&freq=]: a
// share link for a terrain profile. Link-preview crawlers do not run
// JavaScript and never see the part of a URL after '#', so the hash deep
// link #/map?profile=… can only ever preview as the site card. /p serves
// the same SPA shell with per-profile Open Graph tags; a small script in
// index.html moves a browser on to the hash deep link before the app boots.
//
// Properties, each pinned by share_profile_test.go:
//   - Off unless map.dhm.profile.share.enabled (and the profile itself) is
//     on. Off, /p is the plain site document — what it was before #727 —
//     and the handoff script still opens the profile.
//   - One validation path: parseSharedProfile maps profile=<A>,<B> onto the
//     API's a=/b= and hands off to parseTerrainQuery; resolveTerrainRequest
//     then resolves, snaps and gates exactly as /api/terrain/profile does.
//     Hidden, unknown and unpositioned nodes are 404, so a node that cannot
//     be profiled cannot be named in a preview either.
//   - A fixed list of click-tracking keys (fbclid, gclid, utm_*, ...) is
//     ignored: accepted, never validated, never echoed. Any other unknown
//     key is refused.
//   - A refused request gets the plain site document with the error status:
//     nothing from the request is echoed, and a browser still lands on the
//     map, where the panel explains the problem.
//   - No upstream call, no per-client charge: the page needs only the
//     in-memory node snapshot. The verdict and the chart image are #727 M3.
//   - Every request-derived value reaches the document through ogText
//     (og_meta.go); og:url is rebuilt from the parsed values, never copied
//     from the raw query.
//   - The route is exactly /p, one path segment: index.html references its
//     assets relatively, which resolves the same at /p as at /. /p/ is a
//     301 to /p with the query kept.

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/dhm"
)

// sharePath is the share route. It must stay a single path segment with no
// trailing slash: see the package comment above.
const sharePath = "/p"

// shareProfileMaxLen bounds the profile parameter: two 64-hex keys and a
// comma are 129 bytes, the longest valid pair; every other key keeps
// parseTerrainQuery's 64-byte bound.
const shareProfileMaxLen = 140

// shareNameMaxRunes bounds one endpoint name, so both fit in a description.
const shareNameMaxRunes = 48

// shareParams are the only keys /p accepts: the hash link's keys, and v,
// a preview cache-bust (#720 pattern) that is accepted and ignored.
var shareParams = map[string]bool{"profile": true, "model": true, "ha": true, "hb": true, "k": true, "freq": true, "v": true}

// shareTrackingParams are click-tracking keys that chat apps, ad networks
// and newsletters append to a link someone shared (Facebook adds fbclid to
// every outbound click). /p ignores them rather than refusing the page:
// they are never validated, never echoed into a tag or og:url, and the
// index.html handoff never carries them into #/map (#727 owner decision on
// PR #742). The list is fixed and closed, plus any key starting with utm_;
// every other unknown key is still a 400.
var shareTrackingParams = map[string]bool{"fbclid": true, "gclid": true, "dclid": true, "msclkid": true, "igshid": true, "mc_eid": true}

func isShareTrackingParam(k string) bool {
	return shareTrackingParams[k] || strings.HasPrefix(k, "utm_")
}

// registerShareRoutes adds /p and the /p/ redirect. newHTTPRouter calls it
// before the SPA catch-all, which would otherwise answer both.
func (s *Server) registerShareRoutes(router *mux.Router, doc *indexDoc) {
	router.HandleFunc(sharePath, s.handleShareProfile(doc)).Methods("GET", "HEAD")
	router.HandleFunc(sharePath+"/", handleShareSlash).Methods("GET", "HEAD")
}

// handleShareSlash sends /p/ to /p: at /p/ the relative asset URLs of
// index.html would resolve under /p/ and miss.
func handleShareSlash(w http.ResponseWriter, r *http.Request) {
	target := sharePath
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func (s *Server) handleShareProfile(doc *indexDoc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := s.terrain
		if p == nil || !p.set.share.enabled {
			doc.serveSite(w, r, http.StatusOK)
			return
		}
		tq, err := parseSharedProfile(r.URL.Query())
		if err != nil {
			doc.serveSite(w, r, http.StatusBadRequest)
			return
		}
		req, rerr := s.resolveTerrainRequest(p, tq)
		if rerr != nil {
			doc.serveSite(w, r, rerr.status)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Write(doc.og.renderProfile(doc.profile, r, p.sharePage(req)))
	}
}

// parseSharedProfile maps the hash grammar (profile=<A>,<B>) onto the API
// grammar (a=<A>&b=<B>) and hands off to parseTerrainQuery, so the share
// route has no number or coordinate rule of its own.
func parseSharedProfile(q url.Values) (terrainQuery, error) {
	api := url.Values{}
	for k, vs := range q {
		if isShareTrackingParam(k) {
			continue
		}
		limit := 64
		if k == "profile" {
			limit = shareProfileMaxLen
		}
		if !shareParams[k] || len(vs) != 1 || len(vs[0]) > limit {
			return terrainQuery{}, errTerrainQuery
		}
		if k != "profile" && k != "v" {
			api[k] = vs
		}
	}
	refs, ok := splitProfilePair(q.Get("profile"))
	if !ok {
		return terrainQuery{}, errTerrainQuery
	}
	api.Set("a", refs[0])
	api.Set("b", refs[1])
	return parseTerrainQuery(api)
}

// splitProfilePair is public/terrain-profile.js parseProfilePair's split:
// read left to right, a 64-hex key is one endpoint, anything else is a
// lat,lon pair. Anything that does not consume exactly two endpoints is
// rejected whole. The pieces are validated by parseTerrainRef afterwards;
// cmd/server/testdata/profile-param-cases.json pins both parsers to the
// same answers.
func splitProfilePair(raw string) ([2]string, bool) {
	var out [2]string
	tok := strings.Split(raw, ",")
	n, i := 0, 0
	for i < len(tok) {
		if n == 2 {
			return out, false
		}
		if isHexPubkey(strings.ToLower(tok[i])) {
			out[n] = tok[i]
			i++
		} else {
			if i+1 >= len(tok) {
				return out, false
			}
			out[n] = tok[i] + "," + tok[i+1]
			i += 2
		}
		n++
	}
	return out, n == 2
}

// sharePage is the preview text for one resolved request. It reads only
// the request and the operator's radio config: no elevation, so no
// upstream call. The verdict joins the title in #727 M3.
func (p *terrainProfiler) sharePage(req terrainRequest) ogPage {
	overrides := [2]*float64{req.q.ha, req.q.hb}
	pts := [2]dhm.LatLon{req.a, req.b}
	var names [2]string
	assumed := false
	for i, e := range req.ends {
		names[i] = shareEndLabel(e, pts[i])
		if _, _, a := p.antenna(e, overrides[i]); a {
			assumed = true
		}
	}
	model := "terrain + surface"
	if !req.q.both {
		model = "terrain only"
	}
	desc := names[0] + " → " + names[1] + " · " + model
	if assumed {
		// A confident-looking preview over guessed heights is the main way
		// this could mislead, so the caveat is never dropped.
		desc += " · antenna heights assumed"
	}
	return ogPage{
		Title:       "Path profile: " + fmtMetres(round2(req.plan.DistanceM)),
		Description: desc,
		Path:        sharePath + "?" + shareQuery(req.q),
	}
}

// shareEndLabel is public/terrain-profile.js endpointLabel: a node's name,
// or the start of its key when it has none; a point's coordinates. A point
// never gets a name.
func shareEndLabel(e terrainEnd, pt dhm.LatLon) string {
	if e.kind != "node" {
		return fmt.Sprintf("%.4f, %.4f", pt.Lat, pt.Lon)
	}
	if name := ogClip(e.name, shareNameMaxRunes); name != "" {
		return name
	}
	return e.pubkey[:8] + "…"
}

// shareQuery is the canonical query of a parsed request, for og:url: built
// from validated values only. The model is always stated, so the page
// means what the sharer saw whatever the reader's own default model is.
func shareQuery(tq terrainQuery) string {
	var b strings.Builder
	b.WriteString("profile=" + shareRefParam(tq.a) + "," + shareRefParam(tq.b))
	if tq.both {
		b.WriteString("&model=both")
	} else {
		b.WriteString("&model=dtm")
	}
	for _, f := range []struct {
		key string
		v   *float64
	}{{"ha", tq.ha}, {"hb", tq.hb}, {"k", tq.k}, {"freq", tq.freq}} {
		if f.v != nil {
			b.WriteString("&" + f.key + "=" + strconv.FormatFloat(*f.v, 'f', -1, 64))
		}
	}
	return b.String()
}

func shareRefParam(r terrainRef) string {
	if r.pubkey != "" {
		return r.pubkey
	}
	return strconv.FormatFloat(r.pt.Lat, 'f', -1, 64) + "," + strconv.FormatFloat(r.pt.Lon, 'f', -1, 64)
}

// fmtMetres is public/terrain-profile.js fmtMetres, so a preview states a
// distance exactly as the panel it links to: one decimal, in km from 10 km.
func fmtMetres(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "—"
	}
	if math.Abs(v) >= 10000 {
		return strconv.FormatFloat(math.Round(v/1000*10)/10, 'f', -1, 64) + " km"
	}
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) + " m"
}
