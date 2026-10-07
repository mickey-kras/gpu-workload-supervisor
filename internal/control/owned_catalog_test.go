package control

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func ownedLlamaProfile() WorkloadProfile {
	return WorkloadProfile{
		ID:        "vision",
		Label:     "Vision",
		Adapter:   "systemd",
		Unit:      "gws-owned-vision.service",
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service",
		HealthURL: "http://127.0.0.1:9100/health",
		NativeModel: &NativeModel{
			Runtime:      "llama.cpp",
			Instance:     "local",
			Model:        "vision-q8",
			Endpoint:     "http://127.0.0.1:9100",
			LaunchFile:   "/home/u/.config/systemd/user/gws-owned-vision.service",
			LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Owned:        &OwnedLaunch{ModelPath: "/models/vision-q8.gguf", Port: 9100, CtxSize: 8192, GPULayers: 99, Alias: "vision-q8"},
		},
	}
}

func ownedOllamaProfile(id, model string) WorkloadProfile {
	return WorkloadProfile{
		ID:        Workload(id),
		Label:     "Ollama " + model,
		Adapter:   "systemd",
		Unit:      "gws-owned-ollama-local.service",
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-ollama-local.service",
		HealthURL: "http://127.0.0.1:11434/api/tags",
		NativeModel: &NativeModel{
			Runtime:      "ollama",
			Instance:     "local",
			Model:        model,
			Endpoint:     "http://127.0.0.1:11434",
			LaunchFile:   "/home/u/.config/systemd/user/gws-owned-ollama-local.service",
			LaunchSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Owned:        &OwnedLaunch{Port: 11434},
		},
	}
}

func TestOwnedCatalogRequiresVersion2(t *testing.T) {
	c := Catalog{Version: 2, Profiles: []WorkloadProfile{ownedLlamaProfile()}}
	if err := c.Validate(); err != nil {
		t.Fatalf("v2 owned catalog rejected: %v", err)
	}
	c.Version = 1
	if err := c.Validate(); !errors.Is(err, ErrOwnedRequiresV2) {
		t.Fatalf("v1 owned catalog = %v", err)
	}
	c = Catalog{Version: 2, Profiles: []WorkloadProfile{validCatalog().Profiles[0]}}
	if err := c.Validate(); err != nil {
		t.Fatalf("v2 catalog without owned profiles rejected: %v", err)
	}
	for _, version := range []int{0, 3, -1} {
		c.Version = version
		if err := c.Validate(); err == nil || errors.Is(err, ErrOwnedRequiresV2) {
			t.Fatalf("version %d accepted", version)
		}
	}
}

func TestOwnedCatalogStrictDecode(t *testing.T) {
	c := Catalog{Version: 2, Profiles: []WorkloadProfile{ownedLlamaProfile()}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCatalogBytes(raw)
	if err != nil {
		t.Fatalf("v2 owned bytes rejected: %v", err)
	}
	if decoded.Profiles[0].NativeModel.Owned == nil || decoded.Profiles[0].NativeModel.Owned.Port != 9100 {
		t.Fatal("owned spec lost in strict decode")
	}
	tampered := strings.Replace(string(raw), `"port":9100`, `"port":9100,"threads":4`, 1)
	if _, err := DecodeCatalogBytes([]byte(tampered)); err == nil {
		t.Fatal("unknown owned field accepted")
	}
	smuggled := strings.Replace(string(raw), `"version":2`, `"version":1`, 1)
	if _, err := DecodeCatalogBytes([]byte(smuggled)); !errors.Is(err, ErrOwnedRequiresV2) {
		t.Fatalf("owned smuggled into v1 = %v", err)
	}
	if _, err := DecodeCatalogBytes(append(raw, ' ', '{')); err == nil {
		t.Fatal("trailing value accepted")
	}
	if _, err := DecodeCatalog(strings.NewReader(string(raw))); err != nil {
		t.Fatalf("file decode path diverged: %v", err)
	}
}

func TestCatalogCloneDeepCopiesOwned(t *testing.T) {
	c := Catalog{Version: 2, Profiles: []WorkloadProfile{ownedLlamaProfile()}}
	clone := c.Clone()
	c.Profiles[0].NativeModel.Owned.Port = 9200
	if clone.Profiles[0].NativeModel.Owned.Port != 9100 {
		t.Fatal("clone aliases owned launch spec")
	}
}

func TestOwnedPlacementDerivation(t *testing.T) {
	cases := map[string]func(*WorkloadProfile){
		"unit renamed":        func(p *WorkloadProfile) { p.Unit = "vision.service" },
		"unit id mismatch":    func(p *WorkloadProfile) { p.Unit = "gws-owned-other.service" },
		"cgroup foreign":      func(p *WorkloadProfile) { p.Cgroup = "/workloads/vision" },
		"cgroup wrong unit":   func(p *WorkloadProfile) { p.Cgroup = "/user.slice/app.slice/gws-owned-other.service" },
		"launch outside user": func(p *WorkloadProfile) { p.NativeModel.LaunchFile = "/etc/systemd/user/gws-owned-vision.service" },
		"launch name drift":   func(p *WorkloadProfile) { p.NativeModel.LaunchFile = "/home/u/.config/systemd/user/other.service" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := ownedLlamaProfile()
			mutate(&p)
			c := Catalog{Version: 2, Profiles: []WorkloadProfile{p}}
			if err := c.Validate(); err == nil {
				t.Fatal("underived owned placement accepted")
			}
		})
	}
	t.Run("ollama instance unit", func(t *testing.T) {
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{ownedOllamaProfile("chat", "qwen3:latest")}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		p := ownedOllamaProfile("chat", "qwen3:latest")
		p.Unit = "gws-owned-chat.service"
		c.Profiles[0] = p
		if err := c.Validate(); err == nil {
			t.Fatal("ollama owned model unit accepted")
		}
	})
}

func TestOwnedLaunchPerRuntimeAdmissibility(t *testing.T) {
	cases := map[string]func(*WorkloadProfile){
		"privileged port":     func(p *WorkloadProfile) { p.NativeModel.Owned.Port = 80 },
		"endpoint port drift": func(p *WorkloadProfile) { p.NativeModel.Endpoint = "http://127.0.0.1:9200" },
		"endpoint host drift": func(p *WorkloadProfile) { p.NativeModel.Endpoint = "http://localhost:9100" },
		"relative model path": func(p *WorkloadProfile) { p.NativeModel.Owned.ModelPath = "models/vision.gguf" },
		"unclean model path":  func(p *WorkloadProfile) { p.NativeModel.Owned.ModelPath = "/models/../models/v.gguf" },
		"llama max model len": func(p *WorkloadProfile) { p.NativeModel.Owned.MaxModelLen = 4096 },
		"alias control char":  func(p *WorkloadProfile) { p.NativeModel.Owned.Alias = "bad\nalias" },
		"ollama model path": func(p *WorkloadProfile) {
			p.NativeModel.Runtime = "ollama"
			p.NativeModel.Owned.ModelPath = "/models/v.gguf"
		},
		"ollama ctx size":      func(p *WorkloadProfile) { p.NativeModel.Runtime = "ollama"; p.NativeModel.Owned.CtxSize = 8192 },
		"ollama gpu layers":    func(p *WorkloadProfile) { p.NativeModel.Runtime = "ollama"; p.NativeModel.Owned.GPULayers = 1 },
		"ollama max model len": func(p *WorkloadProfile) { p.NativeModel.Runtime = "ollama"; p.NativeModel.Owned.MaxModelLen = 1 },
		"ollama alias":         func(p *WorkloadProfile) { p.NativeModel.Runtime = "ollama"; p.NativeModel.Owned.Alias = "a" },
		"vllm ctx size":        func(p *WorkloadProfile) { p.NativeModel.Runtime = "vllm"; p.NativeModel.Owned.CtxSize = 8192 },
		"vllm gpu layers":      func(p *WorkloadProfile) { p.NativeModel.Runtime = "vllm"; p.NativeModel.Owned.GPULayers = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := ownedLlamaProfile()
			mutate(&p)
			if p.NativeModel.Runtime != "llama.cpp" {
				p.NativeModel.Owned.ModelPath = ""
				p.NativeModel.Owned.CtxSize = 0
				p.NativeModel.Owned.GPULayers = 0
				p.NativeModel.Owned.Alias = ""
				if p.NativeModel.Runtime == "vllm" {
					p.NativeModel.Owned.ModelPath = "/models/vision"
				}
				mutate(&p)
			}
			c := Catalog{Version: 2, Profiles: []WorkloadProfile{p}}
			if err := c.Validate(); err == nil {
				t.Fatal("inadmissible owned spec accepted")
			}
		})
	}
	t.Run("vllm valid", func(t *testing.T) {
		p := ownedLlamaProfile()
		p.NativeModel.Runtime = "vllm"
		p.NativeModel.Owned = &OwnedLaunch{ModelPath: "/models/vision", Port: 9100, MaxModelLen: 4096, Alias: "vision-q8"}
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{p}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("defaults without optional fields", func(t *testing.T) {
		p := ownedLlamaProfile()
		p.NativeModel.Owned = &OwnedLaunch{ModelPath: "/models/vision-q8.gguf", Port: 9100}
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{p}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOwnedEndpointSharingRule(t *testing.T) {
	t.Run("shared owned ollama pair accepted", func(t *testing.T) {
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{ownedOllamaProfile("chat", "qwen3:latest"), ownedOllamaProfile("code", "qwen3-coder:latest")}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cross instance owned endpoint collision", func(t *testing.T) {
		first := ownedOllamaProfile("chat", "qwen3:latest")
		second := ownedOllamaProfile("code", "qwen3-coder:latest")
		second.NativeModel.Instance = "remote"
		second.Unit = "gws-owned-ollama-remote.service"
		second.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-ollama-remote.service"
		second.NativeModel.LaunchFile = "/home/u/.config/systemd/user/gws-owned-ollama-remote.service"
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{first, second}}
		if err := c.Validate(); err == nil {
			t.Fatal("cross-instance owned endpoint collision accepted")
		}
	})
	t.Run("owned adopted endpoint collision", func(t *testing.T) {
		owned := ownedLlamaProfile()
		adopted := WorkloadProfile{
			ID:        "adopted",
			Label:     "Adopted",
			Adapter:   "systemd",
			Unit:      "adopted.service",
			Cgroup:    "/workloads/adopted",
			HealthURL: "http://127.0.0.1:9100/health",
			NativeModel: &NativeModel{
				Runtime:      "llama.cpp",
				Instance:     "other",
				Model:        "vision-q8",
				Endpoint:     "http://127.0.0.1:9100",
				LaunchFile:   "/units/adopted.service",
				LaunchSHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			},
		}
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{owned, adopted}}
		if err := c.Validate(); err == nil {
			t.Fatal("owned-adopted endpoint collision accepted")
		}
	})
	t.Run("owned llama share rejected", func(t *testing.T) {
		first := ownedLlamaProfile()
		second := ownedLlamaProfile()
		second.ID = "vision2"
		second.NativeModel.Model = "vision-q4"
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{first, second}}
		if err := c.Validate(); err == nil {
			t.Fatal("owned llama unit share accepted")
		}
	})
}

func TestOwnedUnitNameDerivation(t *testing.T) {
	if got := OwnedUnitName("ollama", "local", "chat"); got != "gws-owned-ollama-local.service" {
		t.Fatalf("ollama owned unit %q", got)
	}
	if got := OwnedUnitName("llama.cpp", "local", "vision"); got != "gws-owned-vision.service" {
		t.Fatalf("llama owned unit %q", got)
	}
	if got := OwnedUnitName("vllm", "local", "vision"); got != "gws-owned-vision.service" {
		t.Fatalf("vllm owned unit %q", got)
	}
}
