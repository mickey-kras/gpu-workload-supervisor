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

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestDecodeLimitedAcceptsOneStrictValue(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	if err := DecodeLimited(strings.NewReader(`{"a":1}`), 1024, &v); err != nil || v.A != 1 {
		t.Fatalf("decode: %v %#v", err, v)
	}
	if err := DecodeLimited(strings.NewReader(`{"a":1}`+"  \n"), 1024, &v); err != nil {
		t.Fatalf("trailing whitespace: %v", err)
	}
}

func TestDecodeLimitedRejectsUnknownDuplicateAndOversizeInput(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	for _, body := range []string{
		`{"a":1,"b":2}`,
		`{"a":1} {"a":2}`,
		`{"a":`,
		`{"a":"x"}`,
	} {
		if err := DecodeLimited(strings.NewReader(body), 1024, &v); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if err := DecodeLimited(strings.NewReader(`{"a":1,"a":2}`), 1024, &v); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("duplicate key: %v", err)
	}
	if err := DecodeLimited(strings.NewReader(`{"a":12345678}`), 8, &v); err == nil {
		t.Fatal("oversize body accepted")
	}
	if err := DecodeLimited(failingReader{}, 1024, &v); err == nil {
		t.Fatal("read failure ignored")
	}
}
