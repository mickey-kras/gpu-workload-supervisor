package control

import (
	"strings"
	"testing"
)

func TestApplicationAndNativeBindingsShareLaunchEvidenceBoundary(t *testing.T) {
	validEndpoint, validFile, validHash := "http://127.0.0.1:8188", "/units/application.service", strings.Repeat("a", 64)
	tests := []struct {
		name, endpoint, file, hash string
		valid                      bool
	}{
		{"valid", validEndpoint, validFile, validHash, true},
		{"IPv6 loopback", "http://[::1]:8188", validFile, validHash, true},
		{"endpoint path", validEndpoint + "/extra", validFile, validHash, false},
		{"endpoint query", validEndpoint + "?extra=1", validFile, validHash, false},
		{"remote endpoint", "http://192.0.2.1:8188", validFile, validHash, false},
		{"credentials", "http://user:pass@127.0.0.1:8188", validFile, validHash, false},
		{"relative file", validEndpoint, "relative.service", validHash, false},
		{"unclean file", validEndpoint, "/units/../application.service", validHash, false},
		{"missing hash", validEndpoint, validFile, "", false},
		{"invalid hash", validEndpoint, validFile, strings.Repeat("g", 64), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := WorkloadProfile{Adapter: "systemd", HealthURL: tt.endpoint + "/system_stats", LaunchBinding: &LaunchBinding{Runtime: "comfyui", Endpoint: tt.endpoint, LaunchFile: tt.file, LaunchSHA256: tt.hash}}
			native := NativeModel{Runtime: "ollama", Instance: "local", Model: "existing", Endpoint: tt.endpoint, LaunchFile: tt.file, LaunchSHA256: tt.hash}
			appErr, nativeErr := app.validateNativeBinding(), native.validate()
			if (appErr == nil) != tt.valid || (nativeErr == nil) != tt.valid {
				t.Fatalf("application=%v native=%v expected valid=%v", appErr, nativeErr, tt.valid)
			}
		})
	}
	native := NativeModel{Runtime: "arbitrary", Instance: "local", Model: "existing", Endpoint: validEndpoint, LaunchFile: validFile, LaunchSHA256: validHash}
	if err := native.validate(); err == nil {
		t.Fatal("shared evidence bypassed runtime validation")
	}
	native.Runtime = "ollama"
	native.Model = ""
	if err := native.validate(); err == nil {
		t.Fatal("shared evidence bypassed model identity")
	}
	native.Model = "existing"
	native.Owned = &OwnedLaunch{Port: 9000}
	if err := native.validate(); err == nil {
		t.Fatal("shared evidence bypassed owned endpoint validation")
	}
}

func launchBindingProfile() WorkloadProfile {
	return WorkloadProfile{
		ID:        "media",
		Label:     "Media",
		Adapter:   "systemd",
		Unit:      "comfyui.service",
		Cgroup:    "/workloads/comfyui.service",
		HealthURL: "http://127.0.0.1:8188/system_stats",
		LaunchBinding: &LaunchBinding{
			Runtime:      "comfyui",
			Endpoint:     "http://127.0.0.1:8188",
			LaunchFile:   "/units/comfyui.service",
			LaunchSHA256: strings.Repeat("a", 64),
		},
	}
}

func TestLaunchBindingRejectsUnsupportedPlacementAndHealthRoute(t *testing.T) {
	if err := (Catalog{Version: 1, Profiles: []WorkloadProfile{launchBindingProfile()}}).Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*WorkloadProfile){
		"unsupported slice":         func(p *WorkloadProfile) { p.SystemdSlice = "custom.slice" },
		"unsupported slice version": func(p *WorkloadProfile) { p.SystemdSlice, p.SystemdVersion = "app.slice", 250 },
		"binding with native model": func(p *WorkloadProfile) { p.NativeModel = &NativeModel{} },
		"binding with wrong runtime": func(p *WorkloadProfile) {
			p.LaunchBinding.Runtime = "ollama"
		},
		"binding with wrong adapter": func(p *WorkloadProfile) { p.Adapter = AdapterMediaUnload },
		"health route mismatch":      func(p *WorkloadProfile) { p.HealthURL = "http://127.0.0.1:8188/other" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := launchBindingProfile()
			mutate(&p)
			if err := (Catalog{Version: 1, Profiles: []WorkloadProfile{p}}).Validate(); err == nil {
				t.Fatal("unsupported launch binding placement accepted")
			}
		})
	}
}

func TestLaunchBindingEndpointCollisionWithNativeProfile(t *testing.T) {
	native := WorkloadProfile{
		ID:        "text",
		Label:     "Text",
		Adapter:   "systemd",
		Unit:      "llama.service",
		Cgroup:    "/workloads/llama.service",
		HealthURL: "http://127.0.0.1:8188/health",
		NativeModel: &NativeModel{
			Runtime:      nativeRuntimeLlamaCPP,
			Instance:     "local",
			Model:        "existing",
			Endpoint:     "http://127.0.0.1:8188",
			LaunchFile:   "/units/llama.service",
			LaunchSHA256: strings.Repeat("b", 64),
		},
	}
	for _, order := range []string{"native-first", "binding-first"} {
		t.Run(order, func(t *testing.T) {
			profiles := []WorkloadProfile{native, launchBindingProfile()}
			if order == "binding-first" {
				profiles[0], profiles[1] = profiles[1], profiles[0]
			}
			if err := (Catalog{Version: 1, Profiles: profiles}).Validate(); err == nil {
				t.Fatal("launch binding sharing a native endpoint on another unit accepted")
			}
		})
	}
	distinct := launchBindingProfile()
	distinct.LaunchBinding.Endpoint = "http://127.0.0.1:8288"
	distinct.HealthURL = "http://127.0.0.1:8288/system_stats"
	if err := (Catalog{Version: 1, Profiles: []WorkloadProfile{native, distinct}}).Validate(); err != nil {
		t.Fatal(err)
	}
}
