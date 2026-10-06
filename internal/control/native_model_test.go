package control

import "testing"

func TestNativeModelCatalogBinding(t *testing.T) {
	c := validCatalog()
	c.Profiles[0].NativeModel = &NativeModel{Runtime: "ollama", Instance: "local", Model: "a:latest", Endpoint: "http://127.0.0.1:11434", LaunchFile: "/units/a.service", LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
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
		return &NativeModel{Runtime: "ollama", Instance: "local", Model: "a:latest", Endpoint: "http://127.0.0.1:11434", LaunchFile: "/units/a.service", LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	}
	second := func(n *NativeModel) Profile {
		return Profile{ID: "vision", Label: "Vision", Adapter: "systemd", Unit: "vision.service", Cgroup: "/workloads/vision", HealthURL: "http://127.0.0.1:9100/health", NativeModel: n}
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
