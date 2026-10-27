package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeModelCatalogBinding(t *testing.T) {
	c := validCatalog()
	c.Profiles[0].NativeModel = &NativeModel{Runtime: "vllm", Instance: "local", Model: "a:latest", Endpoint: "http://127.0.0.1:8080", LaunchFile: "/units/a.service", LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	clone := c.Clone()
	clone.Profiles[0].NativeModel.Model = "b"
	if c.Profiles[0].NativeModel.Model != "a:latest" {
		t.Fatal("clone aliases model")
	}
	c.Profiles[0].NativeModel.Runtime = "arbitrary"
	if c.Validate() == nil {
		t.Fatal("unsupported native runtime accepted")
	}
}
func TestNativeModelOverlap(t *testing.T) {
	native := func() *NativeModel {
		return &NativeModel{Runtime: "vllm", Instance: "local", Model: "a:latest", Endpoint: "http://127.0.0.1:8080", LaunchFile: "/units/a.service", LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	}
	second := func(n *NativeModel) WorkloadProfile {
		return WorkloadProfile{ID: "vision", Label: "Vision", Adapter: "systemd", Unit: "vision.service", Cgroup: "/workloads/vision", HealthURL: "http://127.0.0.1:9100/health", NativeModel: n}
	}
	cases := map[string]func(*NativeModel){
		"runtime":   func(n *NativeModel) { n.Runtime = "vllm" },
		"endpoint":  func(n *NativeModel) { n.Instance = "remote" },
		"ambiguous": func(n *NativeModel) {},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validCatalog()
			c.Profiles[0].NativeModel = native()
			conflicting := native()
			mutate(conflicting)
			c.Profiles = append(c.Profiles, second(conflicting))
			if c.Validate() == nil {
				t.Fatal("native overlap accepted")
			}
		})
	}
	t.Run("mixed", func(t *testing.T) {
		c := validCatalog()
		c.Profiles = append(c.Profiles, second(native()))
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestNativeModelIdentityRule(t *testing.T) {
	native := func(model string) *NativeModel {
		return &NativeModel{Runtime: "vllm", Instance: "local", Model: model, Endpoint: "http://127.0.0.1:8000", LaunchFile: "/units/a.service", LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	}
	for _, model := range []string{"", " ", "a\x00b", strings.Repeat("m", 1025)} {
		c := validCatalog()
		c.Profiles[0].NativeModel = native(model)
		if c.Validate() == nil || ValidNativeModelIdentity(model) {
			t.Fatalf("invalid model identity %q accepted", model)
		}
	}
	for _, model := range []string{"foo/", "/models/foo", "org/model:tag", strings.Repeat("m", 1024)} {
		c := validCatalog()
		c.Profiles[0].NativeModel = native(model)
		if err := c.Validate(); err != nil || !ValidNativeModelIdentity(model) {
			t.Fatalf("valid model identity %q rejected: %v", model, err)
		}
	}
}

func TestNativeModelRequest(t *testing.T) {
	for _, body := range []string{`{"model":"selected","Model":"other"}`, `{"model":"a","Keep_Alive":0}`, `{"model":"other"}`, `{"model":"a","model":"other"}`, `{"model":"a","keep_alive":0}`, `{}`, `{"model":"a"} {}`} {
		if ValidateModelRequest([]byte(body), "a") == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if err := ValidateModelRequest([]byte(`{"model":"a","messages":[]}`), "a"); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchSourceEvidenceValidationAndClone(t *testing.T) {
	a := LaunchSource{Path: "/opt/unit.d/10-first.conf", SHA256: strings.Repeat("a", 64)}
	b := LaunchSource{Path: "/opt/unit.d/20-second.conf", SHA256: strings.Repeat("b", 64)}
	if err := ValidateLaunchSources("/opt/unit.service", []LaunchSource{a, b}); err != nil {
		t.Fatal(err)
	}
	for _, sources := range [][]LaunchSource{{b, a}, {a, a}, {{Path: "/opt/unsafe.conf", SHA256: "bad"}}, {{Path: "relative.conf", SHA256: a.SHA256}}, {{Path: "/opt/other.d/10-first.conf", SHA256: a.SHA256}, a}} {
		if ValidateLaunchSources("/opt/unit.service", sources) == nil {
			t.Fatal("invalid source evidence accepted", sources)
		}
	}
	c := Catalog{Profiles: []WorkloadProfile{{NativeModel: &NativeModel{DropIns: []LaunchSource{a, b}}}, {LaunchBinding: &LaunchBinding{DropIns: []LaunchSource{a}}}}}
	clone := c.Clone()
	clone.Profiles[0].NativeModel.DropIns[0].SHA256 = "changed"
	clone.Profiles[1].LaunchBinding.DropIns[0].SHA256 = "changed"
	if c.Profiles[0].NativeModel.DropIns[0] != a || c.Profiles[1].LaunchBinding.DropIns[0] != a {
		t.Fatal("clone aliases source evidence")
	}
}

func TestStoredCatalogWithRemovedRuntimeIsRejected(t *testing.T) {
	c := validCatalog()
	c.Profiles[0].NativeModel = &NativeModel{Runtime: "vllm", Instance: "local", Model: "chat", Endpoint: "http://127.0.0.1:8080", LaunchFile: "/units/a.service", LaunchSHA256: strings.Repeat("a", 64)}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCatalogBytes(raw); err != nil {
		t.Fatalf("supported runtime rejected: %v", err)
	}
	owned := `{"version":2,"profiles":[{"id":"vision","label":"Vision","adapter":"systemd","unit":"gws-owned-vision.service","cgroup":"/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service","healthURL":"http://127.0.0.1:9100/health","nativeModel":{"runtime":"llama.cpp","instance":"local","model":"vision","endpoint":"http://127.0.0.1:9100","launchFile":"/home/u/.config/systemd/user/gws-owned-vision.service","launchSHA256":"` + strings.Repeat("a", 64) + `","owned":{"modelPath":"/models/vision.gguf","port":9100,"ctxSize":8192,"gpuLayers":99,"alias":"vision"}}}]}`
	for name, tc := range map[string]struct{ raw, workload, runtime string }{
		"adopted ollama":    {strings.Replace(string(raw), `"runtime":"vllm"`, `"runtime":"ollama"`, 1), string(c.Profiles[0].ID), "ollama"},
		"adopted llama.cpp": {strings.Replace(string(raw), `"runtime":"vllm"`, `"runtime":"llama.cpp"`, 1), string(c.Profiles[0].ID), "llama.cpp"},
		"owned llama.cpp":   {owned, "vision", "llama.cpp"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.raw == string(raw) {
				t.Fatal("fixture did not name the runtime")
			}
			_, err := DecodeCatalogBytes([]byte(tc.raw))
			if err == nil {
				t.Fatal("catalog with removed runtime accepted")
			}
			for _, want := range []string{"workload " + tc.workload, `native runtime "` + tc.runtime + `"`, "no longer supported", "remove or replace this profile"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
		})
	}
	c.Profiles[0].NativeModel.Runtime = "llama.cpp"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `native runtime "llama.cpp"`) {
		t.Fatalf("validation did not name the removed runtime: %v", err)
	}
}
