package profilecard

import (
	"strings"
	"testing"

	"github.com/meshcore-analyzer/copdem"
)

// #759 review finding 2: a mixed DHM + Copernicus card draws both credits,
// each whole, at the credit size, inside the safe area; a Denmark-only card
// draws only the DHM credit, where it always did.
func TestMixedCardDrawsBothCredits(t *testing.T) {
	for _, dark := range []bool{true, false} {
		img, runs, err := render(fxMixed(), [2]string{"Hill Repeater", "Harbour Mast"}, DefaultPalette(dark), Options{Site: "mesh.example"})
		if err != nil {
			t.Fatal(err)
		}
		checkCard(t, "mixed", encodePNG(t, img), runs)
		var dhmRun, fill, site, summary textRun
		for _, r := range runs {
			switch r.Role {
			case "attribution":
				dhmRun = r
			case "attributionFill":
				fill = r
			case "site":
				site = r
			case "summary":
				summary = r
			}
		}
		if fill.Text != FillCredit || fill.Size != attributionSize {
			t.Fatalf("fill credit %+v, want %q at %dpx:\n%s", fill, FillCredit, attributionSize, formatRuns(runs))
		}
		if dhmRun.Text != Credit {
			t.Fatalf("DHM credit missing on a mixed card:\n%s", formatRuns(runs))
		}
		if !(summary.Y < dhmRun.Y && dhmRun.Y < fill.Y) || fill.Y-dhmRun.Y < attributionSize {
			t.Errorf("footer lines overlap or are out of order: summary y=%d, DHM y=%d, fill y=%d", summary.Y, dhmRun.Y, fill.Y)
		}
		if summary.Y-summarySize < footerRule {
			t.Errorf("summary at y=%d crosses the footer rule at %d", summary.Y, footerRule)
		}
		if site.Y != dhmRun.Y {
			t.Errorf("site at y=%d, want the DHM credit line y=%d", site.Y, dhmRun.Y)
		}
	}
}

func TestDenmarkOnlyCardHasNoFillCredit(t *testing.T) {
	_, runs, err := render(fxClear(), [2]string{"A", "B"}, DefaultPalette(true), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Role == "attributionFill" || strings.Contains(r.Text, "Copernicus") {
			t.Errorf("Denmark-only card drew %q", r.Text)
		}
		if r.Role == "attribution" && r.Y != creditBaseline {
			t.Errorf("DHM credit moved to y=%d on a Denmark-only card", r.Y)
		}
		if r.Role == "summary" && r.Y != summaryBaseline {
			t.Errorf("summary moved to y=%d on a Denmark-only card", r.Y)
		}
	}
}

// The card's words are the profile UI's short credit (#779 copdem.Credit),
// built from master's copdem parts.
func TestFillCreditIsTheShortCopernicusLine(t *testing.T) {
	if !strings.Contains(FillCredit, copdem.DatasetName) || !strings.Contains(FillCredit, "COPERNICUS by the European Union and ESA") {
		t.Errorf("FillCredit %q lacks the dataset or the programme", FillCredit)
	}
	if len(FillCredit) >= len(copdem.AttributionDerived) {
		t.Errorf("FillCredit is not the short line")
	}
}
