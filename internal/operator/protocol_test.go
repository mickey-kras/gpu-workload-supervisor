package operator

import (
	"strings"
	"testing"
)

func TestDecodeStrict(t *testing.T) {
	good := `{"protocolVersion":1,"requestId":"r-1","action":"status"}`
	if _, c := Decode([]byte(good)); c != "ok" {
		t.Fatal(c)
	}
	for _, s := range []string{good + ` {}`, strings.Replace(good, `"action"`, `"extra":0,"action"`, 1), strings.Replace(good, `"status"`, `"status","target":"idle"`, 1), strings.Replace(good, `"requestId":"r-1"`, `"requestId":"r-1","requestId":"r-2"`, 1), strings.Repeat(" ", 16385), `{"protocolVersion":1,"requestId":"a","action":"take-control"}`} {
		if _, c := Decode([]byte(s)); c != "invalid_request" {
			t.Fatalf("%s: %s", s, c)
		}
	}
	if _, c := Decode([]byte(strings.Replace(good, `:1`, `:2`, 1))); c != "unsupported_version" {
		t.Fatal(c)
	}
}
func TestDecimalVersion(t *testing.T) {
	base := `{"protocolVersion":1,"requestId":"x","action":"user-switch","target":"third","expected":{"incarnation":"i","version":"VERSION","owner":"user","configurationRevision":"r"}}`
	for _, v := range []string{"1", "18446744073709551615"} {
		if _, c := Decode([]byte(strings.Replace(base, "VERSION", v, 1))); c != "ok" {
			t.Fatal(v, c)
		}
	}
	for _, v := range []string{"0", "01", "-1", "18446744073709551616", "1.0"} {
		if _, c := Decode([]byte(strings.Replace(base, "VERSION", v, 1))); c != "invalid_request" {
			t.Fatal(v, c)
		}
	}
}
func TestRejectCaseVariantsAndExplicitForbiddenFields(t *testing.T) {
	for _, s := range []string{`{"ProtocolVersion":1,"requestId":"r","action":"status"}`, `{"protocolVersion":1,"requestId":"r","action":"status","target":""}`, `{"protocolVersion":1,"requestId":"r","action":"status","expected":null}`, `{"protocolVersion":1,"requestId":"r","action":"take-control","expected":{"Incarnation":"i","version":"1","owner":"supervisor","configurationRevision":"r"}}`} {
		if _, c := Decode([]byte(s)); c != InvalidRequest {
			t.Fatal(s, c)
		}
	}
}
func TestMalformedInputs(t *testing.T) {
	for _, s := range []string{``, `[]`, `null`, `{`, `{"protocolVersion":1,"requestId":"bad id","action":"status"}`, `{"protocolVersion":1,"requestId":"r","action":"exec"}`, `{"protocolVersion":1,"requestId":"r","action":"user-switch","target":"unknown","expected":{}}`, `{"protocolVersion":1,"requestId":"r","action":"user-switch","target":"UPPER","expected":{}}`, `{"protocolVersion":1,"requestId":"r","action":"take-control","expected":{"incarnation":"i","version":"1","owner":"root","configurationRevision":"r"}}`} {
		if _, c := Decode([]byte(s)); c != InvalidRequest {
			t.Fatal(s, c)
		}
	}
}
func TestRequiredProtocolVersion(t *testing.T) {
	for _, s := range []string{`{"requestId":"r","action":"status"}`, `{"protocolVersion":null,"requestId":"r","action":"status"}`} {
		if _, c := Decode([]byte(s)); c != InvalidRequest {
			t.Fatal(s, c)
		}
	}
}
