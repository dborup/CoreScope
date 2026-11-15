package cardkit

import (
	"image/color"
	"reflect"
	"strings"
	"testing"
)

func TestParseCSSColor(t *testing.T) {
	ok := []struct {
		in   string
		want color.NRGBA
	}{
		{"#abc", color.NRGBA{0xaa, 0xbb, 0xcc, 0xff}},
		{"#ABCD", color.NRGBA{0xaa, 0xbb, 0xcc, 0xdd}},
		{"#1d4ed8", color.NRGBA{0x1d, 0x4e, 0xd8, 0xff}},
		{"#1D4ED8", color.NRGBA{0x1d, 0x4e, 0xd8, 0xff}},
		{"#1d4ed880", color.NRGBA{0x1d, 0x4e, 0xd8, 0x80}},
		{"  #fff  ", color.NRGBA{0xff, 0xff, 0xff, 0xff}},
		{"rgb(1, 2, 3)", color.NRGBA{1, 2, 3, 0xff}},
		{"rgb(255,0,128)", color.NRGBA{255, 0, 128, 0xff}},
		{"RGB(10, 20, 30)", color.NRGBA{10, 20, 30, 0xff}},
		// The shipped --terrain-clutter values.
		{"rgba(122, 170, 132, 0.45)", color.NRGBA{122, 170, 132, 115}},
		{"rgba(84, 124, 92, 0.55)", color.NRGBA{84, 124, 92, 140}},
		{"rgba(0, 0, 0, 0)", color.NRGBA{0, 0, 0, 0}},
		{"rgba(0, 0, 0, 1)", color.NRGBA{0, 0, 0, 255}},
		{"rgba(0, 0, 0, .5)", color.NRGBA{0, 0, 0, 128}},
		{"rgba( 74 , 158 , 255 , 0.15 )", color.NRGBA{74, 158, 255, 38}},
	}
	for _, c := range ok {
		got, valid := ParseCSSColor(c.in)
		if !valid || got != c.want {
			t.Errorf("ParseCSSColor(%q) = %v, %v; want %v, true", c.in, got, valid, c.want)
		}
	}
	bad := []string{
		"", "#", "#ab", "#abcde", "#abcdefa", "#ggg", "#12345g", "abc", "red", "transparent",
		"var(--los-line)", "rgb(256, 0, 0)", "rgb(-1, 0, 0)", "rgb(1, 2)", "rgb(1, 2, 3, 4, 5)",
		"rgb(1, 2, 3", "rgb 1, 2, 3)", "rgba(1, 2, 3, 1.5)", "rgba(1, 2, 3, -0.1)", "rgba(1, 2, 3, NaN)",
		"rgba(1, 2, 3, Inf)", "rgba(1, 2, 3, 1e0)", "rgb(1.5, 2, 3)", "rgb(+1, 2, 3)", "rgb(0x1, 2, 3)",
		"rgb(1, 2, 3) x", "#fff;", "hsl(0, 0%, 0%)", "rgba(1, 2, 3, 0.5, 1)", "rgba(1,2,3,)",
		strings.Repeat("#", 200),
	}
	for _, in := range bad {
		if got, valid := ParseCSSColor(in); valid {
			t.Errorf("ParseCSSColor(%q) = %v, true; want invalid", in, got)
		}
	}
}

func TestResolveColors(t *testing.T) {
	var a, b, c color.NRGBA
	a = color.NRGBA{1, 2, 3, 255} // keeps its value: key absent
	fields := []ColorField{{"alpha", &a}, {"beta", &b}, {"gamma", &c}}
	bad := ResolveColors(map[string]string{"beta": "#102030", "gamma": "<script>", "zeta": "#fff"}, fields)
	if a != (color.NRGBA{1, 2, 3, 255}) {
		t.Errorf("absent key changed its field: %v", a)
	}
	if b != (color.NRGBA{0x10, 0x20, 0x30, 0xff}) {
		t.Errorf("beta = %v", b)
	}
	if c != (color.NRGBA{}) {
		t.Errorf("a bad value must leave the field alone, got %v", c)
	}
	if want := []string{"gamma"}; !reflect.DeepEqual(bad, want) {
		t.Errorf("bad = %v, want %v (the key, never the value)", bad, want)
	}
	// Sorted, whatever the field order.
	bad = ResolveColors(map[string]string{"alpha": "x", "gamma": "y", "beta": "z"}, []ColorField{{"gamma", &c}, {"alpha", &a}, {"beta", &b}})
	if want := []string{"alpha", "beta", "gamma"}; !reflect.DeepEqual(bad, want) {
		t.Errorf("bad = %v, want sorted %v", bad, want)
	}
	if bad := ResolveColors(nil, fields); bad != nil {
		t.Errorf("nil section: bad = %v", bad)
	}
}

func TestMergeTheme(t *testing.T) {
	light := map[string]string{"a": "1", "b": "2"}
	dark := map[string]string{"b": "3", "c": "4"}
	if got := MergeTheme(light, dark, false); !reflect.DeepEqual(got, light) {
		t.Errorf("light mode = %v, want theme alone", got)
	}
	got := MergeTheme(light, dark, true)
	if want := map[string]string{"a": "1", "b": "3", "c": "4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dark mode = %v, want %v (Object.assign order)", got, want)
	}
	got["a"] = "changed"
	if light["a"] != "1" || len(dark) != 2 {
		t.Error("MergeTheme modified its inputs")
	}
	if got := MergeTheme(nil, nil, true); len(got) != 0 {
		t.Errorf("nil sections = %v", got)
	}
}
