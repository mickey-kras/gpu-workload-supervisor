package externalcontrol

import (
	"encoding/json"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"strings"
	"testing"
)

const statusBody = `{"apiVersion":"v1","operation":"status"}`
const switchBody = `{"apiVersion":"v1","operation":"request-workload","target":"media","expected":{"incarnation":"inc-1","version":2}}`

func TestDecodeStrictEnvelope(t *testing.T) {
	for _, body := range []string{statusBody, switchBody} {
		req, code := Decode([]byte(body))
		if code != CodeOK || req.APIVersion != "v1" {
			t.Fatalf("valid: %#v %s", req, code)
		}
	}
	for _, body := range []string{
		``, `null`, `[]`, `{}`, statusBody + ` {}`, statusBody + `secret`,
		`{"apiVersion":"v1","operation":"status","operation":"status"}`,
		`{"APIVersion":"v1","operation":"status"}`,
		`{"apiVersion":null,"operation":"status"}`, `{"apiVersion":"v1","operation":null}`,
		`{"apiVersion":"v1","operation":"recovery"}`, `{"apiVersion":"v1","operation":"take-user"}`, `{"apiVersion":"v1","operation":"systemd"}`,
		`{"apiVersion":"v1","operation":"status","identity":"admin-secret"}`,
		`{"apiVersion":"v1","operation":"status","path":"/secret"}`,
		`{"apiVersion":"v1","operation":"status","command":"exec"}`,
		`{"apiVersion":"v1","operation":"status","target":null}`,
		`{"apiVersion":"v1","operation":"status","expected":null}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"unknown","expected":{"incarnation":"i","version":1}}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"media"}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"media","expected":null}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"media","expected":{"incarnation":"i"}}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"media","expected":{"version":1}}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"media","expected":{"incarnation":null,"version":1}}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"media","expected":{"incarnation":"i","version":null}}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"media","expected":{"incarnation":"i","version":0}}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"media","expected":{"incarnation":"i","version":1,"version":2}}`,
		`{"apiVersion":"v1","operation":"request-workload","target":"media","expected":{"incarnation":"i","version":1,"epoch":1}}`,
		strings.Repeat(" ", MaxBodyBytes+1),
	} {
		t.Run(body, func(t *testing.T) {
			_, code := Decode([]byte(body))
			if code != CodeInvalidRequest {
				t.Fatalf("accepted invalid input: %s", code)
			}
		})
	}
	if _, code := Decode([]byte(`{"apiVersion":"v2","operation":"status"}`)); code != CodeUnsupportedVersion {
		t.Fatalf("version: %s", code)
	}
	req, code := Decode([]byte(switchBody))
	if code != CodeOK || req.Target != control.WorkloadMedia || req.Expected.Version != 2 || req.Expected.Incarnation != "inc-1" {
		t.Fatalf("decoded: %#v %s", req, code)
	}
}

func TestResponseHasOnlySanitizedContract(t *testing.T) {
	data, err := json.Marshal(Response{APIVersion: APIVersion, Code: CodeForbidden})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"apiVersion":"v1","code":"forbidden"}` {
		t.Fatalf("response: %s", data)
	}
}
