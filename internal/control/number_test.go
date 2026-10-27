package control

import "testing"

func TestCanonicalUintRejectsNonCanonicalText(t *testing.T) {
	for text, want := range map[string]uint64{"0": 0, "1": 1, "18446744073709551615": 1<<64 - 1} {
		if value, ok := CanonicalUint(text); !ok || value != want {
			t.Fatalf("%q = %d, %v", text, value, ok)
		}
	}
	for _, text := range []string{"", "01", "+1", "-1", " 1", "1 ", "1\n", "0x1", "1e3", "18446744073709551616", "max"} {
		if _, ok := CanonicalUint(text); ok {
			t.Fatalf("accepted %q", text)
		}
	}
}
