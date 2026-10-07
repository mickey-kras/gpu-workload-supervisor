package runtime

import (
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestOllamaMaxLoadedModelsGrammarExtension(t *testing.T) {
	n := control.NativeModel{Runtime: "ollama", Endpoint: "http://localhost:11434", Model: "selected"}
	good := string(nativeLaunchFixture(t, "ollama", n.Endpoint, n.Model))
	extended := strings.Replace(good, "ExecStart=", "Environment=OLLAMA_MAX_LOADED_MODELS=1\nExecStart=", 1)
	if err := qualifyFixtureLaunch([]byte(extended), n); err != nil {
		t.Fatalf("OLLAMA_MAX_LOADED_MODELS=1 rejected: %v", err)
	}
	cases := map[string]string{
		"other value":     strings.Replace(good, "ExecStart=", "Environment=OLLAMA_MAX_LOADED_MODELS=2\nExecStart=", 1),
		"empty value":     strings.Replace(good, "ExecStart=", "Environment=OLLAMA_MAX_LOADED_MODELS=\nExecStart=", 1),
		"duplicated":      strings.Replace(good, "ExecStart=", "Environment=OLLAMA_MAX_LOADED_MODELS=1\nEnvironment=OLLAMA_MAX_LOADED_MODELS=1\nExecStart=", 1),
		"different key":   strings.Replace(good, "ExecStart=", "Environment=OLLAMA_NUM_PARALLEL=1\nExecStart=", 1),
		"lowercase key":   strings.Replace(good, "ExecStart=", "Environment=ollama_max_loaded_models=1\nExecStart=", 1),
		"prefixed key":    strings.Replace(good, "ExecStart=", "Environment=OLLAMA_MAX_LOADED_MODELS_EXTRA=1\nExecStart=", 1),
		"conflicting dup": strings.Replace(good, "ExecStart=", "Environment=OLLAMA_MAX_LOADED_MODELS=1\nEnvironment=OLLAMA_MAX_LOADED_MODELS=2\nExecStart=", 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if err := qualifyFixtureLaunch([]byte(raw), n); err == nil {
				t.Fatal("unsupported environment accepted")
			}
		})
	}
	t.Run("adopt grammar unchanged", func(t *testing.T) {
		if err := qualifyFixtureLaunch([]byte(good), n); err != nil {
			t.Fatalf("baseline ollama adopt unit rejected: %v", err)
		}
	})
	t.Run("non-ollama rejected", func(t *testing.T) {
		server := control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://localhost:9000", Model: "selected"}
		raw := string(nativeLaunchFixture(t, "llama.cpp", server.Endpoint, server.Model))
		raw = strings.Replace(raw, "ExecStart=", "Environment=OLLAMA_MAX_LOADED_MODELS=1\nExecStart=", 1)
		if err := qualifyFixtureLaunch([]byte(raw), server); err == nil {
			t.Fatal("ollama environment accepted for llama.cpp")
		}
	})
}
