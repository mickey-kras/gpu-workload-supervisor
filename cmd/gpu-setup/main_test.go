package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
	"os"
	"strings"
	"testing"
)

const request = `{"version":1,"profile":{"version":1,"statePath":"/tmp/state.db","systemctlPath":"/usr/bin/systemctl","nvidiaSMIPath":"/usr/bin/true","gpuIndex":0,"capacityHeadroomMiB":0},"catalog":{"version":1,"profiles":[{"id":"text","label":"Text","adapter":"systemd","unit":"text.service","cgroup":"/user.slice/text","healthURL":"http://127.0.0.1:8000/health","bootPolicy":"stop-to-idle"}]}}`

func TestRun(t *testing.T) {
	priorHome := homeForSetup
	t.Cleanup(func() { homeForSetup = priorHome })
	home := t.TempDir()
	homeForSetup = func() (string, error) { return home, nil }
	var out bytes.Buffer
	if err := run([]string{"validate"}, strings.NewReader(request), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "operator.json") {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{nil, {"wrong"}, {"validate"}, {"apply"}, {"reconcile"}, {"remove-integration"}} {
		input := "{}"
		if len(args) > 0 && args[0] == "apply" {
			input = request
		}
		if err := run(args, strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatalf("expected unsupported or unavailable: %v", args)
		}
	}
}
func TestPreviewRejectsUnavailableTrustedExecutable(t *testing.T) {
	bad := strings.ReplaceAll(request, "/usr/bin/true", "/missing/nvidia-smi")
	if err := run([]string{"validate"}, strings.NewReader(bad), &bytes.Buffer{}); err == nil {
		t.Fatal("unavailable executable accepted")
	}
}

func TestMainValidate(t *testing.T) {
	priorArgs, priorIn, priorOut := os.Args, os.Stdin, os.Stdout
	t.Cleanup(func() { os.Args = priorArgs; os.Stdin = priorIn; os.Stdout = priorOut })
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	input.WriteString(request)
	input.Seek(0, 0)
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	os.Args = []string{"gpu-setup", "validate"}
	os.Stdin = input
	os.Stdout = output
	main()
}

func TestInjectedCommandBoundaries(t *testing.T) {
	oldHome, oldApply, oldUID, oldDiscover, oldReconcile := homeForSetup, applySetup, effectiveUID, discoverSetup, reconcileSetup
	t.Cleanup(func() {
		homeForSetup = oldHome
		applySetup = oldApply
		effectiveUID = oldUID
		discoverSetup = oldDiscover
		reconcileSetup = oldReconcile
	})
	homeForSetup = func() (string, error) { return "", errors.New("no account") }
	if err := run([]string{"validate"}, strings.NewReader(request), &bytes.Buffer{}); err == nil {
		t.Fatal("missing account")
	}
	homeForSetup = func() (string, error) { return "/home/operator", nil }
	effectiveUID = func() int { return 1000 }
	for _, failure := range []bool{false, true} {
		applySetup = func(context.Context, string, setup.Request) error {
			if failure {
				return errors.New("failed")
			}
			return nil
		}
		err := run([]string{"apply"}, strings.NewReader(request), &bytes.Buffer{})
		if (err != nil) != failure {
			t.Fatal(err)
		}
		discoverSetup = func(context.Context, string) (setup.Discovery, error) {
			if failure {
				return setup.Discovery{}, errors.New("failed")
			}
			return setup.Discovery{}, nil
		}
		err = run([]string{"discover"}, nil, &bytes.Buffer{})
		if (err != nil) != failure {
			t.Fatal(err)
		}
	}
	reconcileSetup = func(context.Context, string) error { return nil }
	if err := run([]string{"reconcile"}, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestDraftCommands(t *testing.T) {
	oldHome, oldUID := homeForSetup, effectiveUID
	t.Cleanup(func() { homeForSetup = oldHome; effectiveUID = oldUID })
	home := t.TempDir()
	homeForSetup = func() (string, error) { return home, nil }
	effectiveUID = func() int { return 1000 }
	var output bytes.Buffer
	if err := run([]string{"drafts"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	input := `{"version":1,"expectedRevision":"","drafts":[{"id":"d","label":"D","app":"ollama"}]}`
	for _, data := range []string{`{`, input + ` {}`, strings.Repeat(" ", 65537), `{"version":2}`, `{"unknown":true}`} {
		if err := run([]string{"save-drafts"}, strings.NewReader(data), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	if err := run([]string{"save-drafts"}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"save-drafts"}, strings.NewReader(input), &output); err == nil {
		t.Fatal("stale accepted")
	}
	effectiveUID = func() int { return 0 }
	if err := run([]string{"save-drafts"}, strings.NewReader(input), &output); err == nil {
		t.Fatal("root accepted")
	}
	if err := os.WriteFile(home+"/.config/gpu-workload-supervisor/drafts.json", []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"drafts"}, strings.NewReader(""), &output); err == nil {
		t.Fatal("invalid file accepted")
	}
}

func TestVerifyBindingsCommand(t *testing.T) {
	old := verifyBindings
	t.Cleanup(func() { verifyBindings = old })
	for _, fail := range []bool{false, true} {
		verifyBindings = func(context.Context, setup.Request) error {
			if fail {
				return errors.New("changed")
			}
			return nil
		}
		if err := run([]string{"verify-bindings"}, strings.NewReader(request), &bytes.Buffer{}); (err != nil) != fail {
			t.Fatalf("failure=%v %v", fail, err)
		}
	}
}
