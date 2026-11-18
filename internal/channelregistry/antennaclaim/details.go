package antennaclaim

// Station details (#898): what an owner submits about their station besides
// the antenna height, proven by the same advert as the height. Every field is
// optional, but a details "set" needs at least one. The details part is one
// unit: a new details set replaces all six fields, both contacts included.

import (
	"errors"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Antenna types and installations, as stored and served.
var (
	AntennaTypes  = []string{"omni", "yagi", "panel", "other"}
	Installations = []string{"roof", "mast", "indoor", "portable"}
	// PowerSources is the canonical order; bit i of a power mask is
	// PowerSources[i] (1 mains, 2 solar, 4 battery).
	PowerSources = []string{"mains", "solar", "battery"}
)

// Bounds of the details fields.
const (
	MinGainDBi = -5.0
	MaxGainDBi = 20.0
	// ReviewGainBelow and ReviewGainAbove flag a gain outside the usual
	// 868 MHz antennas (owner decision Q6).
	ReviewGainBelow = 0.0
	ReviewGainAbove = 15.0
	// MaxContactRunes and MaxContactBytes bound both contacts.
	MaxContactRunes = 32
	MaxContactBytes = 64
)

// StationDetails is the PUBLIC details shape, served by
// GET /api/nodes/{pubkey}/station. It has no admin contact by design: a
// reflection test walks every public response type for one.
type StationDetails struct {
	AntennaType   string   `json:"antennaType,omitempty"`   // omni|yagi|panel|other
	GainDBi       *float64 `json:"gainDbi,omitempty"`       // -5.0..20.0, 0.1 grid
	Power         []string `json:"power,omitempty"`         // subset of mains|solar|battery, canonical order
	Installation  string   `json:"installation,omitempty"`  // roof|mast|indoor|portable
	ContactPublic string   `json:"contactPublic,omitempty"` // opt-in, shown on the node page
}

// DetailsInput is what an owner submits for the details part. Input only:
// never embedded in a response type.
type DetailsInput struct {
	StationDetails
	ContactAdmin string `json:"contactAdmin,omitempty"` // admin queue only
}

// Details validation errors; the server answers them as 400 with the text.
var (
	ErrDetailsEmpty        = errors.New("details must set at least one of antennaType, gainDbi, power, installation, contactPublic or contactAdmin")
	ErrDetailsAntennaType  = errors.New("details.antennaType must be omni, yagi, panel or other")
	ErrDetailsGain         = errors.New("details.gainDbi must be -5 to 20 dBi in steps of 0.1")
	ErrDetailsPower        = errors.New("details.power must be 1-3 different values from mains, solar and battery")
	ErrDetailsInstallation = errors.New("details.installation must be roof, mast, indoor or portable")
	ErrDetailsContact      = errors.New("a contact must be 1-32 letters, digits, single spaces or . _ - @ # + /, without leading or trailing space")
)

// Validate checks d against the shared rules (owner decisions Q6, Q12, N2).
// Values are refused, never rewritten.
func (d DetailsInput) Validate() error {
	if d.AntennaType == "" && d.GainDBi == nil && d.Power == nil && d.Installation == "" &&
		d.ContactPublic == "" && d.ContactAdmin == "" {
		return ErrDetailsEmpty
	}
	if d.AntennaType != "" && !oneOf(d.AntennaType, AntennaTypes) {
		return ErrDetailsAntennaType
	}
	if d.GainDBi != nil && !ValidGain(*d.GainDBi) {
		return ErrDetailsGain
	}
	if d.Power != nil {
		if _, ok := PowerMask(d.Power); !ok {
			return ErrDetailsPower
		}
	}
	if d.Installation != "" && !oneOf(d.Installation, Installations) {
		return ErrDetailsInstallation
	}
	for _, c := range []string{d.ContactPublic, d.ContactAdmin} {
		if c != "" && !ValidContact(c) {
			return ErrDetailsContact
		}
	}
	return nil
}

// ValidGain accepts MinGainDBi–MaxGainDBi in steps of 0.1 dBi.
func ValidGain(g float64) bool {
	if math.IsNaN(g) || g < MinGainDBi-1e-9 || g > MaxGainDBi+1e-9 {
		return false
	}
	t := g * 10
	return math.Abs(t-math.Round(t)) < 1e-6
}

// GainNeedsReview flags a valid gain outside ReviewGainBelow–ReviewGainAbove.
func GainNeedsReview(g float64) bool {
	return g < ReviewGainBelow-1e-9 || g > ReviewGainAbove+1e-9
}

// PowerMask encodes 1–3 different power sources as a bitmask; ok is false
// for an empty list, a duplicate or an unknown value.
func PowerMask(power []string) (mask int, ok bool) {
	if len(power) == 0 || len(power) > len(PowerSources) {
		return 0, false
	}
	for _, p := range power {
		bit := -1
		for i, v := range PowerSources {
			if p == v {
				bit = i
			}
		}
		if bit < 0 || mask&(1<<bit) != 0 {
			return 0, false
		}
		mask |= 1 << bit
	}
	return mask, true
}

// PowerList decodes a power mask into the canonical order; nil for 0.
func PowerList(mask int) []string {
	var out []string
	for i, v := range PowerSources {
		if mask&(1<<i) != 0 {
			out = append(out, v)
		}
	}
	return out
}

// contactPunct is the punctuation a contact may hold besides single spaces.
const contactPunct = "._-@#+/"

// ValidContact checks a contact (public or admin) against the allow-list:
// 1–MaxContactRunes runes and at most MaxContactBytes bytes of Unicode
// letters (L), marks (M), decimal digits (Nd), single U+0020 spaces and
// contactPunct. Everything else is refused, which covers control and format
// characters (Cc, Cf: bidi overrides, zero-width joiners), line and
// paragraph separators, ':' (so no URLs) and markup (<>&"'). A value with a
// leading or trailing space is refused, never trimmed.
func ValidContact(s string) bool {
	if s == "" || len(s) > MaxContactBytes || !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxContactRunes {
		return false
	}
	if s[0] == ' ' || s[len(s)-1] == ' ' {
		return false
	}
	prevSpace := false
	for _, r := range s {
		switch {
		case r == ' ':
			if prevSpace {
				return false
			}
			prevSpace = true
			continue
		case unicode.IsLetter(r), unicode.IsMark(r), unicode.Is(unicode.Nd, r):
		case r < utf8.RuneSelf && strings.IndexByte(contactPunct, byte(r)) >= 0:
		default:
			return false
		}
		prevSpace = false
	}
	return true
}

func oneOf(s string, list []string) bool {
	for _, v := range list {
		if s == v {
			return true
		}
	}
	return false
}
