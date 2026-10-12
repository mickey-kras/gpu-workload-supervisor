package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNumericCUDAVisibilityRemainsUnresolvedWithOneInventoryGPU(t *testing.T) {
	m, runner, p, _ := nativeFixture(t, "llama.cpp", "http://localhost:9000")
	raw, err := os.ReadFile(p.NativeModel.LaunchFile)
	if err != nil {
		t.Fatal(err)
	}
	// Exact reported selection: no ordering setting, plus preserved options
	// and a pre-start check. The query supplies one physical GPU at index 0,
	// but cannot supply evidence about CUDA's logical device mapping.
	raw = append(raw, []byte("Environment=CUDA_VISIBLE_DEVICES=0\nExecStartPre=/usr/bin/true\n")...)
	if err := os.WriteFile(p.NativeModel.LaunchFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	validated := false
	_, err = inspectAutomaticLaunchSourcesWithValidator(p.NativeModel.LaunchFile, "llama.cpp", nil, func(string) error {
		validated = true
		return nil
	})
	if !errors.Is(err, ErrGPUIdentityUnresolved) || !errors.Is(err, ErrLaunchUnsupported) || validated {
		t.Fatalf("numeric inspection executed validation or lost unresolved disposition: %v", err)
	}
	if !strings.Contains(err.Error(), "full UUID") || !strings.Contains(err.Error(), "refresh") || strings.Count(err.Error(), "CUDA_VISIBLE_DEVICES") != 1 {
		t.Fatalf("numeric refusal is not one actionable explanation: %v", err)
	}
	p.NativeModel.GPUUUID = testPhysicalUUID
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	m.config.GPUIndex = 0
	m.config.Catalog.Profiles[2] = p
	query := m.config.NvidiaSMIPath + " --query-gpu=uuid --format=csv,noheader -i 0"
	runner.outputs[query] = []byte(testPhysicalUUID + "\n")
	ctx := context.Background()
	for name, action := range map[string]func() error{
		"start":   func() error { return m.Start(ctx, p.ID) },
		"ready":   func() error { return m.Healthy(ctx, p.ID) },
		"release": func() error { return m.ReleasedFor(ctx, p.ID) },
	} {
		if err := action(); !errors.Is(err, ErrGPUIdentityUnresolved) {
			t.Fatalf("%s inferred CUDA mapping from inventory: %v", name, err)
		}
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " --user start ") {
			t.Fatal("unresolved numeric visibility permitted runtime startup")
		}
	}
	got, err := os.ReadFile(p.NativeModel.LaunchFile)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("numeric refusal changed the external unit", err)
	}
	// Merely supplying an ordering variable is not authoritative mapping
	// evidence; neither admission nor cleanup inspection relaxes the rule.
	for _, order := range []string{"", "Environment=CUDA_DEVICE_ORDER=PCI_BUS_ID\n", "Environment=CUDA_DEVICE_ORDER=FASTEST_FIRST\n"} {
		for _, cleanup := range []bool{false, true} {
			_, err := parseExternalLaunchSourcesMode([][]byte{append(append([]byte(nil), raw...), []byte(order)...)}, "llama.cpp", cleanup)
			if !errors.Is(err, ErrGPUIdentityUnresolved) {
				t.Fatalf("ordering or cleanup relaxed unresolved visibility: %v", err)
			}
		}
	}
}

func TestFullUUIDMappingRemainsConsistentAcrossRuntimeGates(t *testing.T) {
	httpCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"selected"}]}`)
		}
	}))
	defer server.Close()
	m, runner, p, show := nativeFixture(t, "llama.cpp", server.URL)
	raw, err := os.ReadFile(p.NativeModel.LaunchFile)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("Environment=CUDA_VISIBLE_DEVICES="+testPhysicalUUID+"\n")...)
	if err := os.WriteFile(p.NativeModel.LaunchFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p.NativeModel.GPUUUID = testPhysicalUUID
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	m.config.Catalog.Profiles[2] = p
	showWithEnvironment := strings.Replace(show, " -- speech.service", " --property=Environment -- speech.service", 1)
	loaded := append(append([]byte(nil), runner.outputs[show]...), []byte("Environment=CUDA_VISIBLE_DEVICES="+testPhysicalUUID+"\n")...)
	runner.outputs[showWithEnvironment] = loaded
	query := fmt.Sprintf("%s --query-gpu=uuid --format=csv,noheader -i %d", m.config.NvidiaSMIPath, m.config.GPUIndex)
	runner.outputs[query] = []byte(testPhysicalUUID + "\n")
	ctx := context.Background()
	actions := map[string]func() error{
		"preflight": func() error { return m.Preflight(ctx) },
		"start":     func() error { return m.Start(ctx, p.ID) },
		"ready":     func() error { return m.Healthy(ctx, p.ID) },
		"release":   func() error { return m.ReleasedFor(ctx, p.ID) },
	}
	for name, action := range actions {
		if err := action(); err != nil {
			t.Fatalf("%s rejected matching full UUID: %v", name, err)
		}
	}
	for _, evidence := range []struct {
		name, output string
		err          error
	}{
		{name: "mismatch", output: "GPU-ffffffff-ffff-ffff-ffff-ffffffffffff\n"},
		{name: "missing"},
		{name: "ambiguous", output: testPhysicalUUID + "\n" + testPhysicalUUID + "\n"},
		{name: "unavailable", err: errors.New("PRIVATE_QUERY_ERROR")},
	} {
		t.Run(evidence.name, func(t *testing.T) {
			runner.outputs[query] = []byte(evidence.output)
			runner.errs = map[string]error{query: evidence.err}
			beforeHTTP := httpCalls
			for name, action := range actions {
				runner.calls = nil
				if err := action(); !errors.Is(err, ErrLaunchUnsupported) || strings.Contains(err.Error(), "PRIVATE_QUERY_ERROR") {
					t.Fatalf("%s accepted or disclosed failed identity proof: %v", name, err)
				}
				for _, call := range runner.calls {
					if strings.Contains(call, " --user start ") {
						t.Fatal("identity failure permitted startup")
					}
				}
			}
			if httpCalls != beforeHTTP {
				t.Fatal("health endpoint substituted for physical GPU identity evidence")
			}
		})
	}
}
