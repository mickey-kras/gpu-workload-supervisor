package main

import (
	"context"
	"errors"
	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
	"github.com/mickey-kras/llm-workload-supervisor/internal/operator"
	workloadproxy "github.com/mickey-kras/llm-workload-supervisor/internal/proxy"
	"os"
	"path/filepath"
	"testing"
)

func TestRequesterFileDisabledAndRejected(t *testing.T) {
	var c workloadproxy.Config
	if err := configureRequester(&c, "", "unused", operator.LoadProfile); err != nil || c.RequesterPolicy != nil {
		t.Fatal(err)
	}
	if err := configureRequester(&c, "relative", "unused", operator.LoadProfile); err == nil {
		t.Fatal("relative file accepted")
	}
	for _, body := range []string{`{"unknown":true}`, `{"requesters":null} trailing`, string(make([]byte, (1<<20)+1))} {
		path := filepath.Join(t.TempDir(), "requesters.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := configureRequester(&c, path, "unused", operator.LoadProfile); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}

func TestConfigureRequesterProfileBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(`{"requesters":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, state string
		loadErr     error
		valid       bool
	}{
		{"load failure", "state", errors.New("profile"), false},
		{"mismatch", "different", nil, false},
		{"matching", "state", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c workloadproxy.Config
			err := configureRequester(&c, path, "state", func() (operator.Profile, error) { return operator.Profile{StatePath: tc.state}, tc.loadErr })
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
			if tc.valid {
				if _, err := c.ActivateDemand(context.Background(), control.WorkloadMedia, control.State{}, "revision"); err == nil {
					t.Fatal("invalid native service activated")
				}
			}
		})
	}
}

func TestRequesterActivationUsesTypedFence(t *testing.T) {
	state := control.State{Owner: control.OwnerSupervisor, Version: 7, LeaseFence: control.Fence{Incarnation: "initial", Epoch: 1}}
	for _, tc := range []struct {
		name     string
		response operator.Response
		valid    bool
		retry    bool
	}{
		{"success", operator.Response{Code: operator.OK, LeaseFence: &operator.FenceResponse{Incarnation: "fresh", Epoch: "2"}}, true, false},
		{"busy", operator.Response{Code: operator.Busy}, false, true},
		{"deferred", operator.Response{Code: operator.Deferred}, false, true},
		{"stale", operator.Response{Code: operator.StaleState}, false, true},
		{"invalid", operator.Response{Code: operator.InvalidRequest}, false, false},
		{"wrong owner", operator.Response{Code: operator.WrongOwner}, false, false},
		{"recovery", operator.Response{Code: operator.RecoveryRequired}, false, false},
		{"incompatible", operator.Response{Code: operator.IncompatibleConfiguration}, false, false},
		{"gpu in use", operator.Response{Code: operator.GPUInUse}, false, false},
		{"unavailable", operator.Response{Code: operator.Unavailable}, false, false},
		{"missing", operator.Response{Code: operator.OK}, false, false},
		{"bad epoch", operator.Response{Code: operator.OK, LeaseFence: &operator.FenceResponse{Incarnation: "fresh", Epoch: "invalid"}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fence, err := activateRequester(context.Background(), control.WorkloadMedia, state, "revision", func(r operator.Request) operator.Response {
				if r.Action != "activate-workload" || r.Target != control.WorkloadMedia || r.Expected.Version != "7" || r.Expected.ConfigurationRevision != "revision" {
					t.Fatalf("bad activation: %+v", r)
				}
				return tc.response
			})
			if (err == nil) != tc.valid || errors.Is(err, workloadproxy.ErrActivationRetry) != tc.retry {
				t.Fatalf("%+v %v", fence, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := activateRequester(ctx, control.WorkloadMedia, state, "revision", func(operator.Request) operator.Response { t.Fatal("canceled activation"); return operator.Response{} }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
