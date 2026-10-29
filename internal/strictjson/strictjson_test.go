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

func TestCheckMatchesJSONNestingLimit(t *testing.T) {
	for name, delimiters := range map[string][2]string{
		"arrays":  {"[", "]"},
		"objects": {`{"a":`, "}"},
		"mixed":   {`[{"a":`, "}]"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, depth := range []int{10000, 10001} {
				count := depth
				if name == "mixed" {
					count = depth / 2
				}
				body := strings.Repeat(delimiters[0], count) + "0" + strings.Repeat(delimiters[1], count)
				if name == "mixed" && depth%2 != 0 {
					body = "[" + body + "]"
				}
				want := depth == 10000
				if json.Valid([]byte(body)) != want {
					t.Fatalf("encoding/json nesting limit changed at depth %d", depth)
				}
				if err := Check(json.NewDecoder(strings.NewReader(body))); (err == nil) != want {
					t.Fatalf("depth %d: %v, want valid=%v", depth, err, want)
				}
			}
		})
	}
}

func TestCheckConsumesOneValue(t *testing.T) {
	d := json.NewDecoder(strings.NewReader(`{"a":[]} {"b":{}}`))
	for i := 0; i < 2; i++ {
		if err := Check(d); err != nil {
			t.Fatalf("value %d: %v", i, err)
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

func TestWalkVisitsKeysAtAnyDepth(t *testing.T) {
	var keys []string
	err := Walk(json.NewDecoder(strings.NewReader(`{"a":[{"b":{"c":1}}],"d":2}`)), func(key string) error {
		keys = append(keys, key)
		return nil
	})
	if err != nil || strings.Join(keys, ",") != "a,b,c,d" {
		t.Fatalf("keys = %v, err = %v", keys, err)
	}
	stop := errors.New("stop")
	if err := Walk(json.NewDecoder(strings.NewReader(`{"a":{"stop":1}}`)), func(key string) error {
		if key == "stop" {
			return stop
		}
		return nil
	}); !errors.Is(err, stop) {
		t.Fatal("a visit error must stop the walk", err)
	}
}
