package strictjson

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCheckAcceptsUniqueKeysAtAnyDepth(t *testing.T) {
	for _, body := range []string{
		`{"a":1,"b":{"c":2,"d":[3,{"e":4}]},"f":[]}`,
		`[{"a":1},{"a":2}]`,
		`{}`, `[]`, `1`, `"s"`, `null`,
	} {
		if err := Check(json.NewDecoder(strings.NewReader(body))); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
	}
}

func TestCheckRejectsDuplicateKeysAtAnyDepth(t *testing.T) {
	for _, body := range []string{
		`{"a":1,"a":2}`,
		`{"a":{"b":1,"b":2}}`,
		`{"a":[{"b":1,"b":2}]}`,
		`{"a":1,"\u0061":2}`,
	} {
		if err := Check(json.NewDecoder(strings.NewReader(body))); !errors.Is(err, ErrDuplicateKey) {
			t.Fatalf("%s: %v", body, err)
		}
	}
}

func TestCheckRejectsMalformedJSON(t *testing.T) {
	for _, body := range []string{`{"a":`, `{`, `{"a":1`} {
		if err := Check(json.NewDecoder(strings.NewReader(body))); err == nil || errors.Is(err, ErrDuplicateKey) {
			t.Fatalf("%s: %v", body, err)
		}
	}
}
