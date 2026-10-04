package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"
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
