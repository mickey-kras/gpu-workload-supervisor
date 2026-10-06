package control

import (
	"errors"
	"strings"
	"testing"
)

func TestRequestIDAcceptsOpaqueIDs(t *testing.T) {
	for _, id := range []string{"request-1", strings.Repeat("x", MaxRequestIDBytes), strings.Repeat("<", 100), `"quoted\id"`, "café-_\u00e9"} {
		if err := ValidateRequestID(id); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
	}
}

func TestRequestIDRejectsEmptyAndOverlongIDs(t *testing.T) {
	if err := ValidateRequestID(""); err == nil {
		t.Fatal("empty id accepted")
	}
	if err := ValidateRequestID(strings.Repeat("x", MaxRequestIDBytes+1)); !errors.Is(err, ErrRequestIDTooLong) {
		t.Fatalf("overlong id: %v", err)
	}
	if err := ValidateRequestID(strings.Repeat("é", MaxRequestIDBytes/2+1)); !errors.Is(err, ErrRequestIDTooLong) {
		t.Fatalf("multibyte overlong id: %v", err)
	}
}

func TestRequestIDRejectsInvalidUTF8(t *testing.T) {
	if err := ValidateRequestID("invalid\xff"); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestRequestIDRejectsControlCharacters(t *testing.T) {
	for _, id := range []string{"a\nb", "a\rb", "a\tb", "a\x00b", "a\x7fb", "a\u0085b"} {
		if err := ValidateRequestID(id); err == nil {
			t.Fatalf("control character accepted: %q", id)
		}
	}
}
