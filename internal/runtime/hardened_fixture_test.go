package runtime

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/vllmkey"
)

// hardenedFixture holds the chat template and API key files that a hardened
// vLLM launch names.
type hardenedFixture struct {
	template, templateSHA256, keyFile string
}

func newHardenedFixture(t *testing.T) hardenedFixture {
	t.Helper()
	dir := t.TempDir()
	template := filepath.Join(dir, "chat_template.jinja")
	if err := os.WriteFile(template, []byte("{{ messages }}"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := ChatTemplateSHA256(template)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "vllm.env")
	if err := os.WriteFile(keyFile, []byte(vllmkey.Name+"="+strings.Repeat("k", 43)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return hardenedFixture{template: template, templateSHA256: digest, keyFile: keyFile}
}

// args returns the hardened vLLM options after --port.
func (h hardenedFixture) args() string {
	return " --load-format safetensors --chat-template " + h.template
}

// lines returns the hardened [Service] lines.
func (h hardenedFixture) lines() string {
	return "Environment=HF_HUB_OFFLINE=1\nEnvironmentFile=" + h.keyFile + "\n"
}

// loaded returns the systemctl show lines of the hardened settings. The
// Environment line has to be merged with other loaded variables.
func (h hardenedFixture) loaded() string {
	return "EnvironmentFiles=" + h.keyFile + " (ignore_errors=no)\n"
}

func (h hardenedFixture) bind(n control.NativeModel) control.NativeModel {
	n.ChatTemplate, n.ChatTemplateSHA256, n.APIKeyFile = h.template, h.templateSHA256, h.keyFile
	return n
}

func ptrNativeModel(n control.NativeModel) *control.NativeModel {
	return &n
}

// servingBinding returns a hardened binding whose launch file exists, so the
// readiness check can find the model directory and the API key.
func servingBinding(t *testing.T, endpoint, model string) control.NativeModel {
	t.Helper()
	n := control.NativeModel{Runtime: "vllm", Model: model, Endpoint: endpoint}
	raw := nativeLaunchFixture(t, &n)
	n.LaunchFile = filepath.Join(t.TempDir(), "serving.service")
	if err := os.WriteFile(n.LaunchFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return n
}

// servingAPI answers /version like a supported vLLM and requires the fixture
// API key on /v1 routes before it calls next.
func servingAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"version":"0.14.1"}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") && r.Header.Get("Authorization") != "Bearer "+strings.Repeat("k", 43) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
