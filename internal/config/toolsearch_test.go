package config

import "testing"

func TestParseToolSearch(t *testing.T) {
	cases := []struct {
		in      string
		mode    string
		percent int
		ok      bool
	}{
		{"", ToolSearchTST, 0, true},
		{"tst", ToolSearchTST, 0, true},
		{"standard", ToolSearchStandard, 0, true},
		{"auto", ToolSearchAuto, 10, true},
		{"auto:25", ToolSearchAuto, 25, true},
		{"auto:0", ToolSearchTST, 0, true},
		{"auto:100", ToolSearchStandard, 0, true},
		{"auto:150", ToolSearchStandard, 0, true},
		{"auto:abc", "", 0, false},
		{"foo", "", 0, false},
		{"auto:", "", 0, false},
	}
	for _, c := range cases {
		mode, pct, ok := ParseToolSearch(c.in)
		if mode != c.mode || pct != c.percent || ok != c.ok {
			t.Errorf("ParseToolSearch(%q) = (%q,%d,%v), want (%q,%d,%v)", c.in, mode, pct, ok, c.mode, c.percent, c.ok)
		}
	}
}

func TestValidate_ToolSearchEnum(t *testing.T) {
	good := Default()
	good.Tools.ToolSearch = "auto:25"
	if err := Validate(good); err != nil {
		t.Errorf("auto:25 should validate: %v", err)
	}
	bad := Default()
	bad.Tools.ToolSearch = "auto:abc"
	if err := Validate(bad); err == nil {
		t.Error("auto:abc should fail validation")
	}
	bad2 := Default()
	bad2.Tools.ToolSearch = "nonsense"
	if err := Validate(bad2); err == nil {
		t.Error("nonsense should fail validation")
	}
}
