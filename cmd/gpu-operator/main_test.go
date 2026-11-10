package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

func TestRejectsArgumentsWithTypedResponse(t *testing.T) {
	oldArgs, oldOut := os.Args, os.Stdout
	defer func() { os.Args = oldArgs; os.Stdout = oldOut }()
	r, w, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	os.Args = []string{"gpu-operator", "--state=/tmp/untrusted"}
	os.Stdout = w
	main()
	w.Close()
	body, _ := io.ReadAll(r)
	var response struct {
		Code string `json:"code"`
	}
	if e = json.Unmarshal(body, &response); e != nil || response.Code != "invalid_request" {
		t.Fatal(string(body), e)
	}
}

func TestVersionReportsRelease(t *testing.T) {
	for _, flag := range []string{"-version", "--version"} {
		oldArgs, oldOut := os.Args, os.Stdout
		r, w, e := os.Pipe()
		if e != nil {
			t.Fatal(e)
		}
		os.Args = []string{"gpu-operator", flag}
		os.Stdout = w
		main()
		os.Args, os.Stdout = oldArgs, oldOut
		w.Close()
		body, _ := io.ReadAll(r)
		r.Close()
		if string(body) != deployment.Release+"\n" {
			t.Fatalf("%s printed %q", flag, body)
		}
	}
}
