package operator

import (
	"strings"
	"testing"
)

const setIdlePolicyRequest = `{"protocolVersion":1,"requestId":"r","action":"set-idle-policy","expected":{"incarnation":"i","version":"1","owner":"supervisor","configurationRevision":"c"},"settings":{"timeoutMinutes":60,"settingsRevision":"s1"}}`

func TestDecodeGetSettingsIsEnvelopeOnly(t *testing.T) {
	good := `{"protocolVersion":1,"requestId":"r-1","action":"get-settings"}`
	if r, c := Decode([]byte(good)); c != OK || r.Action != actionGetSettings {
		t.Fatal(r, c)
	}
	for _, s := range []string{
		strings.Replace(good, `"action"`, `"expected":null,"action"`, 1),
		strings.Replace(good, `"action"`, `"settings":{"timeoutMinutes":0,"settingsRevision":"s"},"action"`, 1),
		strings.Replace(good, `"action"`, `"target":"idle","action"`, 1),
	} {
		if _, c := Decode([]byte(s)); c != InvalidRequest {
			t.Fatalf("%s: %s", s, c)
		}
	}
}

func TestDecodeSetIdlePolicyRequiresExpectedAndSettings(t *testing.T) {
	r, c := Decode([]byte(setIdlePolicyRequest))
	if c != OK || r.Settings == nil || r.Settings.TimeoutMinutes != 60 || r.Settings.SettingsRevision != "s1" {
		t.Fatal(r, c)
	}
	for name, s := range map[string]string{
		"missing settings":   strings.Replace(setIdlePolicyRequest, `,"settings":{"timeoutMinutes":60,"settingsRevision":"s1"}`, ``, 1),
		"missing expected":   strings.Replace(setIdlePolicyRequest, `"expected":{"incarnation":"i","version":"1","owner":"supervisor","configurationRevision":"c"},`, ``, 1),
		"extra settings key": strings.Replace(setIdlePolicyRequest, `"settingsRevision":"s1"`, `"settingsRevision":"s1","extra":1`, 1),
		"target forbidden":   strings.Replace(setIdlePolicyRequest, `"settings"`, `"target":"idle","settings"`, 1),
		"empty revision":     strings.Replace(setIdlePolicyRequest, `"s1"`, `""`, 1),
		"long revision":      strings.Replace(setIdlePolicyRequest, `"s1"`, `"`+strings.Repeat("x", 129)+`"`, 1),
		"fractional timeout": strings.Replace(setIdlePolicyRequest, `:60`, `:5.5`, 1),
		"string timeout":     strings.Replace(setIdlePolicyRequest, `:60`, `:"60"`, 1),
		"null timeout":       strings.Replace(setIdlePolicyRequest, `:60`, `:null`, 1),
		"null revision":      strings.Replace(setIdlePolicyRequest, `"s1"`, `null`, 1),
		"missing timeout":    strings.Replace(setIdlePolicyRequest, `"timeoutMinutes":60,`, ``, 1),
		"missing revision":   strings.Replace(setIdlePolicyRequest, `,"settingsRevision":"s1"`, ``, 1),
		"boolean timeout":    strings.Replace(setIdlePolicyRequest, `:60`, `:true`, 1),
		"object timeout":     strings.Replace(setIdlePolicyRequest, `:60`, `:{}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, c := Decode([]byte(s)); c != InvalidRequest {
				t.Fatalf("%s: %s", s, c)
			}
		})
	}
}

func TestDecodeSetIdlePolicyTimeoutBounds(t *testing.T) {
	for _, timeout := range []string{"0", "5", "1440"} {
		body := strings.Replace(setIdlePolicyRequest, `:60`, `:`+timeout, 1)
		if _, c := Decode([]byte(body)); c != OK {
			t.Fatalf("timeout %s: %s", timeout, c)
		}
	}
	for _, timeout := range []string{"-1", "1", "4", "1441", "1440000000"} {
		body := strings.Replace(setIdlePolicyRequest, `:60`, `:`+timeout, 1)
		if _, c := Decode([]byte(body)); c != InvalidRequest {
			t.Fatalf("timeout %s: %s", timeout, c)
		}
	}
}

func TestDecodeTransitionActionsRejectSettingsField(t *testing.T) {
	for _, action := range []string{"status", "take-control", "user-switch", "return-control"} {
		base := `{"protocolVersion":1,"requestId":"r","action":"` + action + `","settings":{"timeoutMinutes":0,"settingsRevision":"s"}}`
		if action != "status" {
			base = strings.Replace(base, `"settings"`, `"expected":{"incarnation":"i","version":"1","owner":"user","configurationRevision":"c"},"settings"`, 1)
		}
		if action == "user-switch" {
			base = strings.Replace(base, `"expected"`, `"target":"idle","expected"`, 1)
		}
		if _, c := Decode([]byte(base)); c != InvalidRequest {
			t.Fatalf("%s: %s", action, c)
		}
	}
}
