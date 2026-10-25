package attestation

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestVerifyRejectsExcessiveJSONNesting(t *testing.T) {
	v := fixtureVerifier(t, 1)
	inventory := fixtureInventory(t)
	for _, version := range []int{1, 2} {
		// The envelope object plus 10000 arrays exceeds encoding/json's
		// limit, while remaining well below the envelope byte limit.
		body := `{"version":` + strconv.Itoa(version) + `,"future":` + strings.Repeat("[", 10000) + "0" + strings.Repeat("]", 10000) + "}"
		got := v.Verify(strings.NewReader(body), bytes.NewReader(readFixture(t, "passed.sig.json")), inventory)
		if got.Result() != Malformed || !errors.Is(got.Err(), ErrMalformedEnvelope) {
			t.Fatalf("version %d: %q %v", version, got.Result(), got.Err())
		}
		for _, mode := range []Mode{Advisory, Enforce} {
			if decision := Decide(policy(mode, nil), v, inventory, &got); decision != deny(IntegrityFailure, ReasonMalformed) {
				t.Fatalf("version %d, %s: %#v", version, mode, decision)
			}
		}
	}
}

func TestVerifyPreservesUnsupportedVersionAtJSONNestingLimit(t *testing.T) {
	// A future schema may be deeper than this version's fields. At the
	// standard library's limit it must still be classified by its version.
	body := `{"version":2,"future":` + strings.Repeat("[", 9999) + "0" + strings.Repeat("]", 9999) + "}"
	got := fixtureVerifier(t, 1).Verify(strings.NewReader(body), bytes.NewReader(readFixture(t, "passed.sig.json")), fixtureInventory(t))
	if got.Result() != UnsupportedVersion || !errors.Is(got.Err(), ErrUnsupportedVersion) {
		t.Fatalf("%q %v", got.Result(), got.Err())
	}
}
