package colors

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string]string{"Blue": "blue", " green ": "green", "#a1b2c3": "#A1B2C3", "auto": "", "": ""} {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"chartreuse", "#abc", "#GGGGGG", "rgb(1,2,3)"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestFor(t *testing.T) {
	a, b := For("pagamentos", ""), For("pagamentos", "")
	if a != b {
		t.Fatal("derived color must be stable")
	}
	seen := map[string]bool{}
	for _, name := range []string{"api", "pdv", "pix", "auth", "fiscal", "infra", "site", "bot"} {
		seen[For(name, "").Name] = true
	}
	if len(seen) < 4 {
		t.Fatalf("names should spread over the palette: %v", seen)
	}
	if c := For("x", "red"); c.Name != "red" || c.Emoji != "🟥" {
		t.Fatalf("stored palette color: %+v", c)
	}
	if c := For("x", "#0000F0"); c.Hex != "#0000F0" || c.Emoji != "🟦" {
		t.Fatalf("stored hex color: %+v", c)
	}
	if c := For("x", "not-a-color"); c != For("x", "") {
		t.Fatal("an invalid stored value falls back to the derived color")
	}
}
