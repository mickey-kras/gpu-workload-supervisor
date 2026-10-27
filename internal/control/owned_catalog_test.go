package control

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func ownedVLLMProfile() WorkloadProfile {
	return WorkloadProfile{
		ID:        "vision",
		Label:     "Vision",
		Adapter:   "systemd",
		Unit:      "gws-owned-vision.service",
		Cgroup:    "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service",
		HealthURL: "http://127.0.0.1:9100/health",
		NativeModel: &NativeModel{
			Runtime:      "vllm",
			Instance:     "local",
			Model:        "vision-q8",
			Endpoint:     "http://127.0.0.1:9100",
			LaunchFile:   "/home/u/.config/systemd/user/gws-owned-vision.service",
			LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Owned:        &OwnedLaunch{ModelPath: "/models/vision-q8", Port: 9100, MaxModelLen: 8192, Alias: "vision-q8"},
		},
	}
}

func TestOwnedCatalogRequiresVersion2(t *testing.T) {
	c := Catalog{Version: 2, Profiles: []WorkloadProfile{ownedVLLMProfile()}}
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
	c := Catalog{Version: 2, Profiles: []WorkloadProfile{ownedVLLMProfile()}}
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
	c := Catalog{Version: 2, Profiles: []WorkloadProfile{ownedVLLMProfile()}}
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
			p := ownedVLLMProfile()
			mutate(&p)
			c := Catalog{Version: 2, Profiles: []WorkloadProfile{p}}
			if err := c.Validate(); err == nil {
				t.Fatal("underived owned placement accepted")
			}
		})
	}
}

func TestOwnedLaunchPerRuntimeAdmissibility(t *testing.T) {
	cases := map[string]func(*WorkloadProfile){
		"privileged port":     func(p *WorkloadProfile) { p.NativeModel.Owned.Port = 80 },
		"endpoint port drift": func(p *WorkloadProfile) { p.NativeModel.Endpoint = "http://127.0.0.1:9200" },
		"endpoint host drift": func(p *WorkloadProfile) { p.NativeModel.Endpoint = "http://localhost:9100" },
		"relative model path": func(p *WorkloadProfile) { p.NativeModel.Owned.ModelPath = "models/vision" },
		"unclean model path":  func(p *WorkloadProfile) { p.NativeModel.Owned.ModelPath = "/models/../models/v" },
		"alias control char":  func(p *WorkloadProfile) { p.NativeModel.Owned.Alias = "bad\nalias" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := ownedVLLMProfile()
			mutate(&p)
			c := Catalog{Version: 2, Profiles: []WorkloadProfile{p}}
			if err := c.Validate(); err == nil {
				t.Fatal("inadmissible owned spec accepted")
			}
		})
	}
	t.Run("max model len valid", func(t *testing.T) {
		p := ownedVLLMProfile()
		p.NativeModel.Owned = &OwnedLaunch{ModelPath: "/models/vision", Port: 9100, MaxModelLen: 4096, Alias: "vision-q8"}
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{p}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("defaults without optional fields", func(t *testing.T) {
		p := ownedVLLMProfile()
		p.NativeModel.Owned = &OwnedLaunch{ModelPath: "/models/vision-q8", Port: 9100}
		p.NativeModel.Model = "/models/vision-q8"
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{p}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

// TestOwnedServedModelNameMatchesCatalog locks the render⇄verify invariant at
// admission: a profile whose rendered unit would serve a name other than
// NativeModel.Model could never qualify, so the catalog rejects it up front.
func TestOwnedServedModelNameMatchesCatalog(t *testing.T) {
	t.Run("mismatched alias rejected", func(t *testing.T) {
		p := ownedVLLMProfile()
		p.NativeModel.Owned.Alias = "other-name"
		if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{p}}).Validate(); err == nil {
			t.Fatal("mismatched alias accepted")
		}
	})
	t.Run("empty alias with divergent model path rejected", func(t *testing.T) {
		p := ownedVLLMProfile()
		p.NativeModel.Owned.Alias = ""
		if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{p}}).Validate(); err == nil {
			t.Fatal("model-path fallback diverging from model accepted")
		}
	})
	t.Run("empty alias with model path as identity accepted", func(t *testing.T) {
		p := ownedVLLMProfile()
		p.NativeModel.Owned.Alias = ""
		p.NativeModel.Model = p.NativeModel.Owned.ModelPath
		if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{p}}).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("matching alias accepted", func(t *testing.T) {
		if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{ownedVLLMProfile()}}).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("vllm mismatched served name rejected", func(t *testing.T) {
		p := ownedVLLMProfile()
		p.NativeModel.Runtime = "vllm"
		p.NativeModel.Owned = &OwnedLaunch{ModelPath: "/models/vision", Port: 9100, Alias: "not-vision-q8"}
		if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{p}}).Validate(); err == nil {
			t.Fatal("mismatched vllm served name accepted")
		}
	})
}

// TestOwnedValuesFitCommandGrammar rejects owned values the rendered ExecStart
// could never carry through the launch grammar's tokenization, so admitted
// catalogs cannot produce unqualifiable units.
func TestOwnedValuesFitCommandGrammar(t *testing.T) {
	cases := map[string]func(*OwnedLaunch){
		"whitespace model path":  func(o *OwnedLaunch) { o.ModelPath = "/models/my model" },
		"tab model path":         func(o *OwnedLaunch) { o.ModelPath = "/models/my\tmodel" },
		"newline model path":     func(o *OwnedLaunch) { o.ModelPath = "/models/my\nmodel" },
		"quote model path":       func(o *OwnedLaunch) { o.ModelPath = "/models/we\"ird" },
		"single quote path":      func(o *OwnedLaunch) { o.ModelPath = "/models/we'ird" },
		"backtick model path":    func(o *OwnedLaunch) { o.ModelPath = "/models/we`ird" },
		"dollar model path":      func(o *OwnedLaunch) { o.ModelPath = "/models/$HOME" },
		"percent model path":     func(o *OwnedLaunch) { o.ModelPath = "/models/100%" },
		"backslash model path":   func(o *OwnedLaunch) { o.ModelPath = "/models/win\\path" },
		"semicolon model path":   func(o *OwnedLaunch) { o.ModelPath = "/models/a;b" },
		"whitespace alias":       func(o *OwnedLaunch) { o.Alias = "my model" },
		"quote alias":            func(o *OwnedLaunch) { o.Alias = "mo\"del" },
		"control alias":          func(o *OwnedLaunch) { o.Alias = "bad\x01alias" },
		"specifier escape alias": func(o *OwnedLaunch) { o.Alias = "100%%" },
		"semicolon alias":        func(o *OwnedLaunch) { o.Alias = "a;b" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := ownedVLLMProfile()
			mutate(p.NativeModel.Owned)
			p.NativeModel.Model = p.NativeModel.Owned.Alias
			if p.NativeModel.Model == "" {
				p.NativeModel.Model = p.NativeModel.Owned.ModelPath
			}
			if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{p}}).Validate(); err == nil {
				t.Fatal("grammar-inexpressible owned value accepted")
			}
		})
	}
	t.Run("normal path and alias accepted", func(t *testing.T) {
		p := ownedVLLMProfile()
		p.NativeModel.Owned.ModelPath = "/models/vision-q8_4.0-final"
		p.NativeModel.Owned.Alias = "vision-q8"
		if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{p}}).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("vllm whitespace model path rejected", func(t *testing.T) {
		p := ownedVLLMProfile()
		p.NativeModel.Runtime = "vllm"
		p.NativeModel.Model = "/models/my model"
		p.NativeModel.Owned = &OwnedLaunch{ModelPath: "/models/my model", Port: 9100}
		if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{p}}).Validate(); err == nil {
			t.Fatal("vllm whitespace path accepted")
		}
	})
}

func TestOwnedEndpointSharingRule(t *testing.T) {
	t.Run("cross instance owned endpoint collision", func(t *testing.T) {
		first := ownedVLLMProfile()
		second := ownedVLLMProfile()
		second.ID = "vision2"
		second.NativeModel.Instance = "remote"
		second.Unit = "gws-owned-vision2.service"
		second.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision2.service"
		second.NativeModel.LaunchFile = "/home/u/.config/systemd/user/gws-owned-vision2.service"
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{first, second}}
		if err := c.Validate(); err == nil {
			t.Fatal("cross-instance owned endpoint collision accepted")
		}
	})
	t.Run("owned adopted endpoint collision", func(t *testing.T) {
		owned := ownedVLLMProfile()
		adopted := WorkloadProfile{
			ID:        "adopted",
			Label:     "Adopted",
			Adapter:   "systemd",
			Unit:      "adopted.service",
			Cgroup:    "/workloads/adopted",
			HealthURL: "http://127.0.0.1:9100/health",
			NativeModel: &NativeModel{
				Runtime:      "vllm",
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
	t.Run("owned vllm share rejected", func(t *testing.T) {
		first := ownedVLLMProfile()
		second := ownedVLLMProfile()
		second.ID = "vision2"
		second.NativeModel.Model = "vision-q4"
		second.NativeModel.Owned.Alias = "vision-q4"
		second.Unit = "gws-owned-vision2.service"
		second.Cgroup = "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision2.service"
		second.NativeModel.LaunchFile = "/home/u/.config/systemd/user/gws-owned-vision2.service"
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{first, second}}
		if err := c.Validate(); err == nil {
			t.Fatal("owned vllm endpoint share accepted")
		}
	})
	t.Run("owned and adopted share instance and endpoint", func(t *testing.T) {
		owned := ownedVLLMProfile()
		adopted := WorkloadProfile{
			ID:        "adopted",
			Label:     "Adopted",
			Adapter:   "systemd",
			Unit:      "vllm.service",
			Cgroup:    "/workloads/vllm",
			HealthURL: "http://127.0.0.1:9100/health",
			NativeModel: &NativeModel{
				Runtime:      "vllm",
				Instance:     owned.NativeModel.Instance,
				Model:        "other-model",
				Endpoint:     owned.NativeModel.Endpoint,
				LaunchFile:   "/units/vllm.service",
				LaunchSHA256: strings.Repeat("c", 64),
			},
		}
		c := Catalog{Version: 2, Profiles: []WorkloadProfile{owned, adopted}}
		if err := c.Validate(); err == nil {
			t.Fatal("owned profile shared its endpoint with an adopted profile")
		}
	})
}

// TestNativeEndpointNormalizedCollision compares endpoints by socket address,
// never by stored text: padded ports and equivalent loopback spellings collide.
func TestNativeEndpointNormalizedCollision(t *testing.T) {
	adopted := func(id, instance, endpoint string) WorkloadProfile {
		return WorkloadProfile{
			ID:        Workload(id),
			Label:     "Model " + id,
			Adapter:   "systemd",
			Unit:      "model-" + id + ".service",
			Cgroup:    "/workloads/model-" + id,
			HealthURL: endpoint + "/health",
			NativeModel: &NativeModel{
				Runtime:      "vllm",
				Instance:     instance,
				Model:        "m-" + id,
				Endpoint:     endpoint,
				LaunchFile:   "/units/model-" + id + ".service",
				LaunchSHA256: strings.Repeat("2", 64),
			},
		}
	}
	t.Run("zero padded port collides", func(t *testing.T) {
		a := adopted("a", "one", "http://127.0.0.1:9100")
		b := adopted("b", "two", "http://127.0.0.1:09100")
		if err := (Catalog{Version: 1, Profiles: []WorkloadProfile{a, b}}).Validate(); err == nil {
			t.Fatal("zero-padded port endpoint accepted as distinct")
		}
	})
	t.Run("ipv6 loopback forms collide", func(t *testing.T) {
		a := adopted("a", "one", "http://[::1]:9100")
		b := adopted("b", "two", "http://[0:0:0:0:0:0:0:1]:9100")
		if err := (Catalog{Version: 1, Profiles: []WorkloadProfile{a, b}}).Validate(); err == nil {
			t.Fatal("expanded IPv6 loopback accepted as distinct")
		}
	})
	t.Run("hostname casing collides", func(t *testing.T) {
		a := adopted("a", "one", "http://LOCALHOST:9100")
		b := adopted("b", "two", "http://localhost:9100")
		if err := (Catalog{Version: 1, Profiles: []WorkloadProfile{a, b}}).Validate(); err == nil {
			t.Fatal("case-variant hostname accepted as distinct")
		}
	})
	t.Run("distinct ports accepted", func(t *testing.T) {
		a := adopted("a", "one", "http://127.0.0.1:9100")
		b := adopted("b", "two", "http://127.0.0.1:9101")
		if err := (Catalog{Version: 1, Profiles: []WorkloadProfile{a, b}}).Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("stored endpoint never rewritten", func(t *testing.T) {
		c := Catalog{Version: 1, Profiles: []WorkloadProfile{adopted("a", "one", "http://127.0.0.1:9100")}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if c.Profiles[0].NativeModel.Endpoint != "http://127.0.0.1:9100" {
			t.Fatalf("endpoint rewritten to %q", c.Profiles[0].NativeModel.Endpoint)
		}
	})
}

func TestOwnedUnitNameDerivation(t *testing.T) {
	if got := OwnedUnitName("vision"); got != "gws-owned-vision.service" {
		t.Fatalf("owned unit %q", got)
	}
}
