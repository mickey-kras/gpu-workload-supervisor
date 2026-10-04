package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEntryTypedFailures(t *testing.T) {
	for _, x := range []struct {
		body string
		args []string
		want Code
	}{{"", []string{"--state=/tmp/x"}, InvalidRequest}, {"bad\n", nil, InvalidRequest}, {`{"protocolVersion":2,"requestId":"r","action":"status"}` + "\n", nil, UnsupportedVersion}, {`{"protocolVersion":1,"requestId":"r","action":"status"}` + "\n", nil, IncompatibleConfiguration}} {
		var out bytes.Buffer
		Serve(x.args, strings.NewReader(x.body), &out, func() (Profile, error) { return Profile{}, errors.New("private") }, func() error { return nil })
		var r Response
		if e := json.Unmarshal(out.Bytes(), &r); e != nil || r.Code != x.want || r.Status != nil {
			t.Fatal(out.String(), e)
		}
	}
}
func TestEntryDetachFailureAndNativeOpen(t *testing.T) {
	body := `{"protocolVersion":1,"requestId":"r","action":"status"}` + "\n"
	for _, fail := range []bool{true, false} {
		var out bytes.Buffer
		Serve(nil, strings.NewReader(body), &out, func() (Profile, error) { return Profile{}, nil }, func() error {
			if fail {
				return errors.New("session")
			}
			return nil
		})
		var r Response
		if e := json.Unmarshal(out.Bytes(), &r); e != nil || r.Code != Unavailable || r.RequestID != "r" {
			t.Fatal(out.String(), e)
		}
	}
}
