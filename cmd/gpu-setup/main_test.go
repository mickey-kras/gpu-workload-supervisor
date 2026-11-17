package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

const request = `{"version":1,"profile":{"version":1,"statePath":"/tmp/state.db","systemctlPath":"/usr/bin/systemctl","nvidiaSMIPath":"/usr/bin/true","gpuIndex":0,"capacityHeadroomMiB":0},"catalog":{"version":1,"profiles":[{"id":"text","label":"Text","adapter":"systemd","unit":"text.service","cgroup":"/user.slice/text","healthURL":"http://127.0.0.1:8000/health","bootPolicy":"stop-to-idle"}]}}`

func TestVersionReportsReleaseWithoutAccount(t *testing.T) {
	actions := systemActions()
	actions.home = func() (string, error) { return "", errors.New("no account") }
	actions.euid = func() int { return 0 }
	for _, flag := range []string{"-version", "--version"} {
		var output bytes.Buffer
		if err := actions.run([]string{flag}, strings.NewReader(""), &output); err != nil || output.String() != deployment.Release+"\n" {
			t.Fatalf("%s = %q, %v", flag, output.String(), err)
		}
	}
	if err := actions.run([]string{"-version", "apply"}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("version combined with an action: %v", err)
	}
}

func TestRun(t *testing.T) {
	actions := systemActions()
	home := t.TempDir()
	actions.home = func() (string, error) { return home, nil }
	actions.euid = func() int { return 1000 }
	var out bytes.Buffer
	if err := actions.run([]string{"validate"}, strings.NewReader(request), &out); err != nil {
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
		if err := actions.run(args, strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatalf("expected unsupported or unavailable: %v", args)
		}
	}
}
func TestPreviewRejectsUnavailableTrustedExecutable(t *testing.T) {
	actions := systemActions()
	actions.euid = func() int { return 1000 }
	bad := strings.ReplaceAll(request, "/usr/bin/true", "/missing/nvidia-smi")
	if err := actions.run([]string{"validate"}, strings.NewReader(bad), &bytes.Buffer{}); err == nil {
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
	actions := systemActions()
	actions.home = func() (string, error) { return "", errors.New("no account") }
	if err := actions.run([]string{"validate"}, strings.NewReader(request), &bytes.Buffer{}); err == nil {
		t.Fatal("missing account")
	}
	actions.home = func() (string, error) { return "/home/operator", nil }
	actions.euid = func() int { return 1000 }
	for _, failure := range []bool{false, true} {
		actions.apply = func(context.Context, string, setup.Request) error {
			if failure {
				return errors.New("failed")
			}
			return nil
		}
		err := actions.run([]string{"apply"}, strings.NewReader(request), &bytes.Buffer{})
		if (err != nil) != failure {
			t.Fatal(err)
		}
		actions.discover = func(context.Context, string) (setup.Discovery, error) {
			if failure {
				return setup.Discovery{}, errors.New("failed")
			}
			return setup.Discovery{}, nil
		}
		err = actions.run([]string{"discover"}, nil, &bytes.Buffer{})
		if (err != nil) != failure {
			t.Fatal(err)
		}
	}
	actions.reconcile = func(context.Context, string) error { return nil }
	if err := actions.run([]string{"reconcile"}, nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	called := false
	actions.policyTick = func(context.Context, string) error { called = true; return nil }
	if err := actions.run([]string{"idle-policy-tick"}, nil, &bytes.Buffer{}); err != nil || !called {
		t.Fatalf("idle-policy-tick dispatch: %v called=%v", err, called)
	}
}

func TestDraftCommands(t *testing.T) {
	actions := systemActions()
	home := t.TempDir()
	actions.home = func() (string, error) { return home, nil }
	actions.euid = func() int { return 1000 }
	var output bytes.Buffer
	if err := actions.run([]string{"drafts"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	input := `{"version":1,"expectedRevision":"","drafts":[{"id":"d","label":"D","app":"vllm"}]}`
	for _, data := range []string{`{`, input + ` {}`, strings.Repeat(" ", 65537), `{"version":2}`, `{"unknown":true}`} {
		if err := actions.run([]string{"save-drafts"}, strings.NewReader(data), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	if err := actions.run([]string{"save-drafts"}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if err := actions.run([]string{"save-drafts"}, strings.NewReader(input), &output); err == nil {
		t.Fatal("stale accepted")
	}
	actions.euid = func() int { return 0 }
	if err := actions.run([]string{"save-drafts"}, strings.NewReader(input), &output); err == nil {
		t.Fatal("root accepted")
	}
	actions.euid = func() int { return 1000 }
	if err := os.WriteFile(home+"/.config/gpu-workload-supervisor/drafts.json", []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := actions.run([]string{"drafts"}, strings.NewReader(""), &output); err == nil {
		t.Fatal("invalid file accepted")
	}
}

func TestModelStatusCommand(t *testing.T) {
	actions := systemActions()
	actions.home = func() (string, error) { return t.TempDir(), nil }
	actions.euid = func() int { return 1000 }
	var output bytes.Buffer
	limit := `{"version":1}` + strings.Repeat(" ", setup.MaxModelStatusRequestBytes-len(`{"version":1}`))
	if err := actions.run([]string{"model-status"}, strings.NewReader(limit), &output); err != nil || output.String() != `{"version":1,"models":[]}`+"\n" {
		t.Fatalf("model-status: %q %v", output.String(), err)
	}
	for _, data := range []string{`{`, `{"version":2}`, `{"version":1,"unknown":true}`, `{"version":1,"recheck":["../x"]}`, `{"version":1}` + strings.Repeat(" ", setup.MaxModelStatusRequestBytes)} {
		if err := actions.run([]string{"model-status"}, strings.NewReader(data), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %.40s", data)
		}
	}
}

func TestModelStatusTimeout(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), recheckTimeout)
	defer cancel()
	for _, tc := range []struct {
		request setup.ModelStatusRequest
		limit   time.Duration
	}{
		{setup.ModelStatusRequest{Version: 1}, actionTimeout},
		{setup.ModelStatusRequest{Version: 1, Recheck: []string{"text"}}, recheckTimeout},
	} {
		ctx, stop := modelStatusContext(parent, tc.request)
		deadline, ok := ctx.Deadline()
		stop()
		if left := time.Until(deadline); !ok || left > tc.limit || left < tc.limit-time.Minute {
			t.Fatalf("recheck %v: %v left", tc.request.Recheck, left)
		}
	}
}

func TestVerifyBindingsCommand(t *testing.T) {
	actions := systemActions()
	actions.euid = func() int { return 1000 }
	for _, fail := range []bool{false, true} {
		actions.verify = func(context.Context, setup.Request) error {
			if fail {
				return errors.New("changed")
			}
			return nil
		}
		if err := actions.run([]string{"verify-bindings"}, strings.NewReader(request), &bytes.Buffer{}); (err != nil) != fail {
			t.Fatalf("failure=%v %v", fail, err)
		}
	}
}

func TestRootRejectedForAllSetupActions(t *testing.T) {
	actions := systemActions()
	actions.home = func() (string, error) { return t.TempDir(), nil }
	actions.euid = func() int { return 0 }
	for _, action := range []string{"discover", "probe", "fingerprint", "save-drafts", "verify-bindings", "validate", "apply", "reconcile", "idle-policy-tick"} {
		if err := actions.run([]string{action}, strings.NewReader(request), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not root") {
			t.Fatalf("%s accepted as root: %v", action, err)
		}
	}
}

func TestAllDesktopCommandsRejectRootBeforeReadingInputOrWritingOutput(t *testing.T) {
	for _, command := range []string{
		"correct-gpu", "discover", "probe", "prepare", "fingerprint", "render-owned", "drafts", "save-drafts", "verify-bindings", "validate", "apply", "reconcile", "idle-policy-tick", "remove-integration", "model-status", "model-check", "model-review", "model-approve",
	} {
		t.Run(command, func(t *testing.T) {
			a := systemActions()
			home := t.TempDir()
			a.home = func() (string, error) { return home, nil }
			a.euid = func() int { return 0 }
			input := strings.NewReader("invalid request")
			var output bytes.Buffer
			err := a.run([]string{command}, input, &output)
			if err == nil || err.Error() != "run guided setup as the desktop account, not root" {
				t.Fatalf("root guard: %v", err)
			}
			if input.Len() != len("invalid request") || output.Len() != 0 {
				t.Fatal("root command consumed input or emitted output")
			}
			files, err := os.ReadDir(home)
			if err != nil || len(files) != 0 {
				t.Fatalf("root command changed home: %v %v", files, err)
			}
		})
	}
}

func TestCorrectGPURejectsMalformedRequestWithoutOutput(t *testing.T) {
	actions := systemActions()
	home := t.TempDir()
	actions.home = func() (string, error) { return home, nil }
	actions.euid = func() int { return 1000 }
	var out bytes.Buffer
	if err := actions.run([]string{"correct-gpu"}, strings.NewReader(`{"unknown":true}`), &out); err == nil {
		t.Fatal("malformed GPU correction request was accepted")
	}
	if out.Len() != 0 {
		t.Fatalf("rejected GPU correction wrote output: %q", out.String())
	}
	files, err := os.ReadDir(home)
	if err != nil || len(files) != 0 {
		t.Fatalf("rejected GPU correction changed home: %v %v", files, err)
	}
}

func TestDraftCommandsRecoverUnsupportedSelections(t *testing.T) {
	for _, app := range []string{"ollama", "llama.cpp"} {
		t.Run(app, func(t *testing.T) {
			home := t.TempDir()
			root := home + "/.config/gpu-workload-supervisor"
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			data := `{"version":1,"drafts":[{"id":"old","label":"Old","app":"` + app + `"},{"id":"keep","label":"Keep","app":"comfyui"}]}`
			if err := os.WriteFile(root+"/drafts.json", []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := runDrafts(home, &output); err != nil {
				t.Fatal(err)
			}
			var snapshot struct {
				setup.DraftSnapshot
				Warning string `json:"warning"`
			}
			if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Revision == "" || snapshot.Warning == "" || len(snapshot.Drafts) != 2 {
				t.Fatalf("missing recovery snapshot: %+v", snapshot)
			}
			request := setup.DraftRequest{Version: 1, ExpectedRevision: snapshot.Revision, Drafts: snapshot.Drafts[1:]}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			output.Reset()
			if err := runSaveDrafts(home, bytes.NewReader(encoded), &output); err != nil {
				t.Fatal(err)
			}
			if err := runSaveDrafts(home, bytes.NewReader(encoded), &bytes.Buffer{}); err == nil {
				t.Fatal("stale recovery revision accepted")
			}
			current, err := setup.ReadDrafts(home)
			if err != nil || len(current.Drafts) != 1 || current.Drafts[0].ID != "keep" {
				t.Fatalf("supported selection lost: %+v %v", current, err)
			}
		})
	}
}

// The model check actions read strict JSON of at most 4 KiB and write
// nothing for a request that they refuse.
func TestModelCheckActionsRefuseInvalidRequests(t *testing.T) {
	actions := systemActions()
	home := t.TempDir()
	actions.home = func() (string, error) { return home, nil }
	actions.euid = func() int { return 1000 }
	revision := strings.Repeat("a", 40)
	for action, tc := range map[string]struct{ raw, want string }{
		"model-check":   {`{"version":1,"workload":"chat","source":{"repo":"org/model","revision":"` + revision + `"},"extra":1}`, "unknown field"},
		"model-review":  {`{"version":1,"workload":"chat"}` + strings.Repeat(" ", 4096), "json exceeds"},
		"model-approve": {`{"version":1,"workload":"chat","chatTemplateSHA256":"x","reviewSHA256":"y"}`, setup.ErrModelCheckRequest.Error()},
	} {
		var output bytes.Buffer
		if err := actions.run([]string{action}, strings.NewReader(tc.raw), &output); err == nil || !strings.Contains(err.Error(), tc.want) || output.Len() != 0 {
			t.Fatalf("%s: %v %q", action, err, output.String())
		}
	}
	if err := runModelAction(context.Background(), home, "model-other", strings.NewReader("{}"), &bytes.Buffer{}); err == nil {
		t.Fatal("an unknown model action ran")
	}
	// A valid request for setup without a profile names no workload.
	valid := map[string]string{
		"model-check":   `{"version":1,"workload":"chat","source":{"repo":"org/model","revision":"` + revision + `"}}`,
		"model-review":  `{"version":1,"workload":"chat"}`,
		"model-approve": `{"version":1,"workload":"chat","chatTemplateSHA256":"` + strings.Repeat("a", 64) + `","reviewSHA256":"` + strings.Repeat("b", 64) + `"}`,
	}
	for action, raw := range valid {
		var output bytes.Buffer
		if err := actions.run([]string{action}, strings.NewReader(raw), &output); !errors.Is(err, setup.ErrModelNotConfigured) || output.Len() != 0 {
			t.Fatalf("%s: %v %q", action, err, output.String())
		}
	}
	files, err := os.ReadDir(home)
	if err != nil || len(files) != 0 {
		t.Fatalf("a refused request changed home: %v %v", files, err)
	}
}

// A model check gets the longest timeout, and a stop signal cancels it.
func TestModelCheckContext(t *testing.T) {
	for action, want := range map[string]time.Duration{"model-check": modelCheckTimeout, "model-status": recheckTimeout, "model-review": actionTimeout} {
		ctx, cancel := actionContext(action)
		deadline, ok := ctx.Deadline()
		if left := time.Until(deadline); !ok || left > want || left < want-time.Minute {
			t.Fatalf("%s deadline %v", action, left)
		}
		cancel()
		if ctx.Err() == nil {
			t.Fatalf("%s: cancel did not end the context", action)
		}
	}
	ctx, cancel := actionContext("model-check")
	defer cancel()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a stop signal did not cancel the model check")
	}
}
