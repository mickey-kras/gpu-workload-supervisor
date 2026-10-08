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
