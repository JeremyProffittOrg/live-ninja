package clientver

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		raw     string
		wantOK  bool
		surface string
	}{
		{"web/0.9.0+g1a2b3c4", true, "web"},
		{"android/2.1.0+r48", true, "android"},
		{"m5stack/1.4.2+20260717-1", true, "m5stack"},
		// The live Android build sends this today. The grammar has no room
		// for a pre-release suffix, so it does NOT parse — which is why the
		// Azure gate cannot be keyed on version alone (gap register F3).
		{"android/0.2.2-hal+r5", false, ""},
		{"", false, ""},
		{"android/2.1.0", false, ""},
		{"desktop/1.0.0+x", false, ""},
	}
	for _, tc := range cases {
		got, ok := Parse(tc.raw)
		if ok != tc.wantOK {
			t.Errorf("Parse(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
			continue
		}
		if ok && got.Surface != tc.surface {
			t.Errorf("Parse(%q) surface = %q, want %q", tc.raw, got.Surface, tc.surface)
		}
	}
}

func TestAtLeast(t *testing.T) {
	v, ok := Parse("android/2.1.3+r48")
	if !ok {
		t.Fatal("fixture did not parse")
	}
	cases := []struct {
		major, minor, patch int
		want                bool
	}{
		{2, 1, 3, true}, {2, 1, 2, true}, {2, 0, 9, true}, {1, 9, 9, true},
		{2, 1, 4, false}, {2, 2, 0, false}, {3, 0, 0, false},
	}
	for _, tc := range cases {
		if got := v.AtLeast(tc.major, tc.minor, tc.patch); got != tc.want {
			t.Errorf("2.1.3.AtLeast(%d,%d,%d) = %v, want %v", tc.major, tc.minor, tc.patch, got, tc.want)
		}
	}
}
