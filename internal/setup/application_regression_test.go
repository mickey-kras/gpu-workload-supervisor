package setup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoveryPythonModuleIsNotModel(t *testing.T) {
	launch := "ExecStart={ path=/usr/bin/python3 ; argv[]=/usr/bin/python3 -m vllm.entrypoints.openai.api_server --port 8000 ; }"
	got := launchModels("vllm", launch)
	if len(got) != 0 {
		t.Fatalf("invented model from Python module: %+v", got)
	}
}

func TestOversizedInventoryIsBounded(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"data":[`)
	for i := 0; i < 50000; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"id":"m%d"}`, i)
	}
	body.WriteString(`]}`)
	inventory := body.String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"version":"1"}`)
			return
		}
		fmt.Fprint(w, inventory)
	}))
	defer server.Close()
	got, err := Probe(context.Background(), ProbeRequest{App: "vllm", Endpoint: server.URL})
	if err != nil || got.InventoryStatus != "invalid" || len(got.Models) != 0 {
		t.Fatalf("oversized inventory accepted: status=%s models=%d error=%v", got.InventoryStatus, len(got.Models), err)
	}
}

func TestServedAliasCannotCollideWithModelRoot(t *testing.T) {
	server := probeServer(t, map[string]string{"/version": `{"version":"1"}`, "/v1/models": `{"data":[{"id":"a","root":"alias:b"},{"id":"b"}]}`})
	got, err := Probe(context.Background(), ProbeRequest{App: "vllm", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("unrelated identities collapsed without shared root: %+v", got)
	}
}

func TestDiscoveryConversionHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := candidate(ProbeRequest{App: "vllm", Endpoint: "http://127.0.0.1"})
	if err := servedCandidates(ctx, []servedModel{{ID: "model"}}, &result); err != context.Canceled {
		t.Fatalf("cancelled conversion: %v", err)
	}
	if len(result.Models) != 0 {
		t.Fatal("cancelled conversion added models")
	}
}
func TestDiscoveryModelCardinalityBound(t *testing.T) {
	if err := inventoryBound(context.Background(), 4096); err != nil {
		t.Fatal(err)
	}
	if err := inventoryBound(context.Background(), 4097); err == nil {
		t.Fatal("inventory limit not enforced")
	}
}
func TestVLLMPythonLaunchUsesExplicitModel(t *testing.T) {
	launch := "ExecStart={ path=/usr/bin/python3 ; argv[]=/usr/bin/python3 -m vllm.entrypoints.openai.api_server --model /models/real --port 8000 ; }"
	got := launchModels("vllm", launch)
	if len(got) != 1 || got[0].ID != "/models/real" {
		t.Fatalf("explicit launch model not preserved: %+v", got)
	}
}

func TestProbeRejectsEmptyQueryAndFragmentOrigins(t *testing.T) {
	for _, suffix := range []string{"?", "#", "/?", "/#"} {
		_, err := Probe(context.Background(), ProbeRequest{App: "vllm", Endpoint: "http://127.0.0.1" + suffix})
		if err == nil {
			t.Fatalf("accepted origin suffix %q", suffix)
		}
	}
}
