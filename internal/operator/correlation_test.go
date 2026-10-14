package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestRejectedRequestCorrelation(t *testing.T) {
	for _, x := range []struct {
		name, body, id string
		code           Code
	}{
		{"unsupported version", `{"protocolVersion":2,"requestId":"r-1","action":"status"}`, "r-1", UnsupportedVersion},
		{"zero version", `{"protocolVersion":0,"requestId":"r-1","action":"status"}`, "r-1", UnsupportedVersion},
		{"negative version", `{"protocolVersion":-1,"requestId":"r-1","action":"status"}`, "r-1", UnsupportedVersion},
		{"maximum ID", `{"protocolVersion":2,"requestId":"` + strings.Repeat("r", 64) + `","action":"status"}`, strings.Repeat("r", 64), UnsupportedVersion},
		{"unsupported action", `{"protocolVersion":1,"requestId":"r-1","action":"exec"}`, "r-1", InvalidRequest},
		{"unsupported mutation action", `{"protocolVersion":1,"requestId":"r-1","action":"exec","expected":{"incarnation":"i","version":"1","owner":"user","configurationRevision":"r"}}`, "r-1", InvalidRequest},
		{"forbidden action field", `{"protocolVersion":1,"requestId":"r-1","action":"exec","target":"idle"}`, "r-1", InvalidRequest},
		{"missing ID", `{"protocolVersion":1,"action":"status"}`, "", InvalidRequest},
		{"null ID", `{"protocolVersion":1,"requestId":null,"action":"status"}`, "", InvalidRequest},
		{"numeric ID", `{"protocolVersion":1,"requestId":1,"action":"status"}`, "", InvalidRequest},
		{"invalid ID", `{"protocolVersion":1,"requestId":"bad id","action":"status"}`, "", InvalidRequest},
		{"long ID", `{"protocolVersion":1,"requestId":"` + strings.Repeat("r", 65) + `","action":"status"}`, "", InvalidRequest},
		{"invalid ID unsupported version", `{"protocolVersion":2,"requestId":"bad id","action":"status"}`, "", InvalidRequest},
		{"duplicate ID", `{"protocolVersion":1,"requestId":"r-1","requestId":"r-2","action":"status"}`, "", InvalidRequest},
		{"alias ID", `{"protocolVersion":1,"RequestId":"spoofed","action":"status"}`, "", InvalidRequest},
		{"uppercase ID", `{"protocolVersion":1,"REQUESTID":"spoofed","action":"status"}`, "", InvalidRequest},
		{"alias after canonical ID", `{"protocolVersion":1,"requestId":"canonical","RequestId":"spoofed","action":"status"}`, "", InvalidRequest},
		{"alias before canonical ID", `{"protocolVersion":1,"RequestId":"spoofed","requestId":"canonical","action":"status"}`, "", InvalidRequest},
		{"unknown field", `{"protocolVersion":1,"requestId":"r-1","action":"status","extra":1}`, "", InvalidRequest},
		{"trailing document", `{"protocolVersion":1,"requestId":"r-1","action":"status"} {}`, "", InvalidRequest},
	} {
		t.Run(x.name, func(t *testing.T) {
			req, code := Decode([]byte(x.body))
			if code != x.code || !reflect.DeepEqual(req, Request{RequestID: x.id}) {
				t.Fatalf("Decode = %+v, %s", req, code)
			}
			var out bytes.Buffer
			err := Serve(nil, strings.NewReader(x.body+"\n"), &out,
				func() (Profile, error) { t.Fatal("rejected request loaded profile"); return Profile{}, nil },
				func() error { t.Fatal("rejected request detached session"); return nil })
			want, _ := json.Marshal(Response{ProtocolVersion: 1, RequestID: x.id, Code: x.code})
			if err != nil || out.String() != string(want)+"\n" {
				t.Fatalf("Serve = %q, %v", out.String(), err)
			}
		})
	}
}

func TestStrictClientAcceptsDeliveredErrors(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for the GNOME contract parser")
	}
	for _, x := range []struct {
		version int
		action  string
		code    Code
	}{{2, "status", UnsupportedVersion}, {1, "exec", InvalidRequest}, {1, "status", IncompatibleConfiguration}} {
		body, _ := json.Marshal(Request{ProtocolVersion: x.version, RequestID: "r-1", Action: x.action})
		var out bytes.Buffer
		err := Serve(nil, bytes.NewReader(append(body, '\n')), &out,
			func() (Profile, error) { return Profile{}, errors.New("private profile error") }, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("node", "--input-type=module", "-e", `
import {readFileSync} from 'node:fs';
import {parseResponse} from '../../clients/gnome/gpu-workload-supervisor@local/contract.js';
process.stdout.write(parseResponse(readFileSync(0, 'utf8'), 'r-1', process.argv[1]).code);
`, x.action)
		cmd.Stdin = &out
		parsed, err := cmd.CombinedOutput()
		if err != nil || string(parsed) != string(x.code) {
			t.Fatalf("strict client parsed %q, %v; want %s", parsed, err, x.code)
		}
	}
}

func TestResponseWriteFailureDoesNotReplayApplicationWork(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	loads, detaches := 0, 0
	err = Serve(nil, strings.NewReader(`{"protocolVersion":1,"requestId":"r-1","action":"status"}`+"\n"), w,
		func() (Profile, error) { loads++; return Profile{}, errors.New("configuration") },
		func() error { detaches++; return nil })
	if !errors.Is(err, syscall.EPIPE) || loads != 1 || detaches != 1 {
		t.Fatalf("write failure = %v, loads = %d, detaches = %d", err, loads, detaches)
	}
}
