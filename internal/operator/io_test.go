package operator

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBoundedInput(t *testing.T) {
	for _, s := range []string{"{}", strings.Repeat("x", MaxRequestBytes+1), "{}\n{}\n"} {
		if _, e := ReadRequest(strings.NewReader(s), time.Second); e == nil {
			t.Fatal("bad framing accepted")
		}
	}
	r, w, _ := os.Pipe()
	defer w.Close()
	defer r.Close()
	start := time.Now()
	if _, e := ReadRequest(r, 10*time.Millisecond); e == nil || time.Since(start) > time.Second {
		t.Fatal("unbounded input", e)
	}
}
func TestOutputFailureDoesNotRunAgain(t *testing.T) {
	r, w, _ := os.Pipe()
	r.Close()
	defer w.Close()
	if WriteResponse(w, Response{ProtocolVersion: 1, Code: OK}, time.Second) == nil {
		t.Fatal("closed reader ignored")
	}
	var b bytes.Buffer
	if e := WriteResponse(&b, Response{ProtocolVersion: 1, Code: OK}, time.Second); e != nil || !bytes.HasSuffix(b.Bytes(), []byte("\n")) {
		t.Fatal(e)
	}
	blockedR, blockedW := io.Pipe()
	defer blockedR.Close()
	defer blockedW.Close()
	if WriteResponse(blockedW, Response{ProtocolVersion: 1, Code: OK}, 10*time.Millisecond) == nil {
		t.Fatal("unbounded output")
	}
}
