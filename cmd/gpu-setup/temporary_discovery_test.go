package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
	"strings"
	"testing"
	"time"
)

func TestTemporaryCommandsKeepDesktopAccountGuard(t *testing.T) {
	a := systemActions()
	a.home = func() (string, error) { return t.TempDir(), nil }
	a.euid = func() int { return 0 }
	for _, action := range []string{"temporary-discover", "temporary-status", "temporary-cleanup"} {
		if err := a.run([]string{action}, strings.NewReader(`{}`), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not root") {
			t.Fatalf("%s bypassed root guard: %v", action, err)
		}
	}
}
func TestTemporaryStatusSupportsReadOnlyFirstSetup(t *testing.T) {
	a := systemActions()
	a.home = func() (string, error) { return t.TempDir(), nil }
	a.euid = func() int { return 1000 }
	var out bytes.Buffer
	if err := a.run([]string{"temporary-status"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"available":false`) || !strings.Contains(out.String(), "read-only") {
		t.Fatal(out.String())
	}
	for _, action := range []string{"temporary-discover", "temporary-cleanup"} {
		if err := a.run([]string{action}, strings.NewReader(`{"unsupported":true}`), &bytes.Buffer{}); err == nil {
			t.Fatalf("%s accepted malformed request", action)
		}
	}
}

// Temporary commands share the setup action boundaries used by other CLI
// operations. Dispatch preserves the typed request and bounded context, and
// encodes the backend result even when cleanup reports an actionable error.
func TestTemporaryCommandsDispatchTypedContracts(t *testing.T) {
	expected := operator.Expected{Incarnation: "instance", Version: "42", Owner: control.OwnerSupervisor, ConfigurationRevision: "catalog"}
	session := &control.TemporaryDiscoverySession{ID: "session", Token: "private-token", Status: "cleanup_required", InvocationID: "invocation"}
	status := setup.TemporaryDiscoveryStatus{Session: session, Expected: &expected, Available: false, Reason: "cleanup required"}
	completed := setup.TemporaryDiscoveryResult{Session: &control.TemporaryDiscoverySession{ID: "session", Status: "completed"}, Models: []setup.ModelCandidate{{ID: "local-model", Locality: "local"}}}
	failedCleanup := setup.TemporaryDiscoveryResult{Session: session, Error: "GPU release remains unverified"}
	tests := []struct {
		action, body string
		response     any
	}{
		{"temporary-status", "", status},
		{"temporary-discover", `{"unit":"ollama.service","expected":{"incarnation":"instance","version":"42","owner":"supervisor","configurationRevision":"catalog"},"consent":true,"externalControlPaused":true}`, completed},
		{"temporary-cleanup", `{"id":"session","token":"private-token","externalControlPaused":true}`, failedCleanup},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			a := systemActions()
			home := t.TempDir()
			a.home = func() (string, error) { return home, nil }
			a.euid = func() int { return 1000 }
			calls := 0
			checkContext := func(ctx context.Context, gotHome string) {
				t.Helper()
				calls++
				if gotHome != home {
					t.Fatalf("account home changed: %q", gotHome)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Minute {
					t.Fatal("unbounded operation context")
				}
			}
			a.temporaryStatus = func(ctx context.Context, gotHome string) (setup.TemporaryDiscoveryStatus, error) {
				checkContext(ctx, gotHome)
				return status, nil
			}
			a.temporaryDiscover = func(ctx context.Context, gotHome string, r setup.TemporaryDiscoveryRequest) (setup.TemporaryDiscoveryResult, error) {
				checkContext(ctx, gotHome)
				if r.Unit != "ollama.service" || r.Expected != expected || !r.Consent || !r.ExternalControlPaused {
					t.Fatalf("request changed: %+v", r)
				}
				return completed, nil
			}
			a.temporaryCleanup = func(ctx context.Context, gotHome string, r setup.TemporaryCleanupRequest) (setup.TemporaryDiscoveryResult, error) {
				checkContext(ctx, gotHome)
				if r.ID != "session" || r.Token != "private-token" || !r.ExternalControlPaused {
					t.Fatalf("cleanup authorization changed: %+v", r)
				}
				return failedCleanup, nil
			}
			var out bytes.Buffer
			if err := a.run([]string{tt.action}, strings.NewReader(tt.body), &out); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("backend calls=%d", calls)
			}
			want, err := json.Marshal(tt.response)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(out.String()) != string(want) {
				t.Fatalf("response changed: %s", out.String())
			}
		})
	}
}

func TestTemporaryCommandsPropagateBackendErrorsWithoutSuccessPayload(t *testing.T) {
	failure := errors.New("deployment verification failed")
	for _, action := range []string{"temporary-status", "temporary-discover", "temporary-cleanup"} {
		t.Run(action, func(t *testing.T) {
			a := systemActions()
			a.home = func() (string, error) { return t.TempDir(), nil }
			a.euid = func() int { return 1000 }
			calls := 0
			a.temporaryStatus = func(context.Context, string) (setup.TemporaryDiscoveryStatus, error) {
				calls++
				return setup.TemporaryDiscoveryStatus{}, failure
			}
			a.temporaryDiscover = func(context.Context, string, setup.TemporaryDiscoveryRequest) (setup.TemporaryDiscoveryResult, error) {
				calls++
				return setup.TemporaryDiscoveryResult{}, failure
			}
			a.temporaryCleanup = func(context.Context, string, setup.TemporaryCleanupRequest) (setup.TemporaryDiscoveryResult, error) {
				calls++
				return setup.TemporaryDiscoveryResult{}, failure
			}
			var out bytes.Buffer
			if err := a.run([]string{action}, strings.NewReader(`{}`), &out); !errors.Is(err, failure) {
				t.Fatalf("backend error hidden: %v", err)
			}
			if calls != 1 || out.Len() != 0 {
				t.Fatalf("failure emitted success: calls=%d body=%q", calls, out.String())
			}
		})
	}
}
