package main

import (
	"bytes"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"io"
	"strings"
	"testing"
)

func TestFingerprintProtocol(t *testing.T) {
	old := inspectLaunch
	t.Cleanup(func() { inspectLaunch = old })
	inspectLaunch = func(path string, binding control.NativeModel) (string, error) {
		if path != "/trusted/model.service" || binding.Model != "chosen" {
			return "", errors.New("invalid binding")
		}
		return "computed", nil
	}
	input := `{"binding":{"launchFile":"/trusted/model.service","model":"chosen"}}`
	var output bytes.Buffer
	if err := run([]string{"fingerprint"}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"sha256":"computed"`) {
		t.Fatal(output.String())
	}
	for _, invalid := range []string{`{`, `{}`, input + ` {}`, `{"extra":1}`, strings.Repeat(" ", 16385)} {
		if err := fingerprint(strings.NewReader(invalid), io.Discard); err == nil {
			t.Fatalf("accepted %q", invalid[:min(len(invalid), 80)])
		}
	}
	if err := fingerprint(brokenFingerprintIO{}, io.Discard); err == nil {
		t.Fatal("read error ignored")
	}
	if err := fingerprint(strings.NewReader(input), brokenFingerprintIO{}); err == nil {
		t.Fatal("write error ignored")
	}
}

type brokenFingerprintIO struct{}

func (brokenFingerprintIO) Read([]byte) (int, error)  { return 0, errors.New("read failed") }
func (brokenFingerprintIO) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestFingerprintRejectsUnqualifiedLaunch(t *testing.T) {
	if err := fingerprint(strings.NewReader(`{"binding":{"launchFile":"/missing/service"}}`), io.Discard); err == nil {
		t.Fatal("missing launch file accepted")
	}
}
