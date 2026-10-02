package runtime

import (
	"context"
	"errors"
	"fmt"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCommandErrorsRedactOutputAndKeepCause(t *testing.T) {
	const secret = "sentinel-secret\x1b[31m\n"
	cause := fmt.Errorf("%s: %w", secret, context.DeadlineExceeded)
	for _, action := range []string{"inspect", "stop", "capacity", "parse"} {
		t.Run(action, func(t *testing.T) {
			runner := &fakeRunner{outputs: map[string][]byte{textShowCommand: []byte(secret), "/usr/bin/true --user stop -- text.service": []byte(secret), gpuFreeCommand: []byte(secret)}, errs: map[string]error{}}
			if action != "parse" {
				for key := range runner.outputs {
					runner.errs[key] = cause
				}
			}
			config := testConfig()
			config.TextRequiredMiB = 1
			manager, err := newSystemdManager(config, runner, http.DefaultClient)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "inspect":
				_, err = manager.Observe(context.Background())
			case "stop":
				err = manager.Stop(context.Background(), control.WorkloadText)
			default:
				err = manager.capacity(context.Background(), control.WorkloadText)
			}
			if err == nil || strings.Contains(err.Error(), "sentinel") || strings.ContainsAny(err.Error(), "\x1b\n") {
				t.Fatalf("unsafe error: %v", err)
			}
			if action != "parse" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost cause: %v", err)
			}
			if (action == "capacity" || action == "parse") && !errors.Is(err, ErrCapacity) {
				t.Fatalf("lost category: %v", err)
			}
		})
	}
}

func TestExecRunnerDistinguishesCancellationFromExitFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := (ExecRunner{}).Run(ctx, "/usr/bin/sleep", "10"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline identity: %v", err)
	}
	if _, err := (ExecRunner{}).Run(context.Background(), "/usr/bin/false"); err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ordinary exit misclassified: %v", err)
	}
}

func TestInvalidSystemdPropertiesDoNotExposeOutput(t *testing.T) {
	const secret = "sentinel-property\x1b[31m"
	for _, property := range []string{"LoadState", "ActiveState", "SubState", "ControlGroup", "duplicate"} {
		t.Run(property, func(t *testing.T) {
			output := "LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/text.service\n"
			if property == "duplicate" {
				output += secret + "=one\n" + secret + "=two\n"
			} else {
				values := map[string]string{"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "ControlGroup": "/workloads/text.service"}
				output = strings.Replace(output, property+"="+values[property], property+"="+secret, 1)
			}
			r := stoppedRunner()
			r.outputs[textShowCommand] = []byte(output)
			m := strictManager(t, r)
			_, err := m.Observe(context.Background())
			if err == nil || strings.Contains(err.Error(), "sentinel") || strings.ContainsAny(err.Error(), "\x1b\n") {
				t.Fatalf("unsafe property diagnostic: %v", err)
			}
			if !strings.Contains(err.Error(), "text.service") {
				t.Fatalf("lost configured unit: %v", err)
			}
		})
	}
}
