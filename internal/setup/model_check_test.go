package setup

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
)

func TestModelCheckEndToEndAndErrorPaths(t *testing.T) {
	rev := strings.Repeat("a", 40)
	req, err := DecodeModelCheckRequest(strings.NewReader(`{"version":1,"workload":"vision","source":{"repo":"org/model","revision":"` + rev + `"}}`))
	if err != nil || req.Workload != "vision" {
		t.Fatalf("DecodeModelCheckRequest = %+v, %v", req, err)
	}
	if _, err := DecodeModelCheckRequest(strings.NewReader(`{"version":2}`)); !errors.Is(err, ErrModelCheckRequest) {
		t.Fatalf("expected ErrModelCheckRequest, got %v", err)
	}
	if _, err := ModelCheck(context.Background(), t.TempDir(), req, &http.Client{}); !errors.Is(err, ErrModelNotConfigured) {
		t.Fatalf("expected ErrModelNotConfigured, got %v", err)
	}

	backend, home, applyReq := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	modelDir := profile.NativeModel.Owned.ModelPath
	tplContent := "{{ messages }}"
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"architectures":["Qwen3"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "chat_template.jinja"), []byte(tplContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile.NativeModel.ChatTemplate, []byte(tplContent), 0o600); err != nil {
		t.Fatal(err)
	}
	profile.NativeModel.ChatTemplateSHA256 = modelcheck.PinTemplate(tplContent)
	refingerprint(t, &profile)

	applyReq.Catalog = control.Catalog{Version: control.CatalogVersionOwned, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(context.Background(), home, applyReq); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	entry, err := ModelCheck(context.Background(), home, req, &http.Client{})
	if err != nil || entry.Status != modelcheck.StatusPassed || !entry.StaticChecksPassed || entry.LaunchBlocked {
		t.Fatalf("ModelCheck = %+v, %v", entry, err)
	}

	report, err := ModelStatus(context.Background(), home, ModelStatusRequest{Version: 1, Recheck: []string{"vision"}})
	if err != nil || len(report.Models) != 1 || report.Models[0].Status != modelcheck.StatusPassed || !report.Models[0].Rechecked {
		t.Fatalf("ModelStatus with recheck = %+v, %v", report, err)
	}
}
