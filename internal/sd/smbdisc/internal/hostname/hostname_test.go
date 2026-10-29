package hostname

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"OFFICE-PC", true},
		{"ANNA'S-PC", true},
		{"nas.local", true},
		{"", false},
		{".hidden", false},
		{"../../etc", false},
		{"a b", false},
		{"bad\xffname", false},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
	} {
		if got := Valid(tc.name); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
