package runtime

import (
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

func TestFullUUIDMappingRemainsConsistentAcrossRuntimeGates(t *testing.T) {
	httpCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"selected"}]}`)
		}
	}))
	defer server.Close()
	m, runner, p, show := nativeFixture(t, "vllm", server.URL)
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
