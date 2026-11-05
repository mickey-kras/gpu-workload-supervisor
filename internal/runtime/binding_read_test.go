package runtime

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// A failed read in the binding check proves no change, so it is not
// definite. A missing file stays definite. A name longer than the file
// system allows gives an I/O error other than a missing file.
func TestBindingReadFailureIsNotDefinite(t *testing.T) {
	unreadable := "/" + strings.Repeat("r", 300)
	missing := filepath.Join(t.TempDir(), "missing")
	for name, err := range map[string]error{
		"chat template": func() error { _, err := ChatTemplateSHA256(unreadable); return err }(),
		"executable":    validateNativeExecutable(unreadable),
		"API key file":  checkKeyFile(unreadable),
		"model":         serverModelCheck(t, unreadable),
	} {
		if !errors.Is(err, ErrLaunchUnsupported) || !errors.Is(err, errBindingRead) || DefiniteBindingFailure(err) {
			t.Errorf("%s with a failed read = %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"chat template": func() error { _, err := ChatTemplateSHA256(missing); return err }(),
		"executable":    validateNativeExecutable(missing),
		"API key file":  checkKeyFile(missing),
	} {
		if errors.Is(err, errBindingRead) || !DefiniteBindingFailure(err) {
			t.Errorf("%s that is missing = %v", name, err)
		}
	}
	// The mark does not change the text.
	if err := checkKeyFile(unreadable); !strings.HasPrefix(err.Error(), "native launch requires a supported direct local command: API key file: ") ||
		!strings.HasSuffix(err.Error(), ": missing or broken link") {
		t.Errorf("text of a failed read = %q", err)
	}
}

// serverModelCheck qualifies a vLLM launch whose model path is model.
func serverModelCheck(t *testing.T, model string) error {
	t.Helper()
	unit, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nExecStart=/usr/bin/vllm serve " + model + " --served-model-name selected --port 9000 --host 127.0.0.1\n")})
	if err != nil {
		t.Fatal(err)
	}
	return qualifyParsedMainCommand(unit, control.NativeModel{Runtime: "vllm", Model: "selected", Endpoint: "http://127.0.0.1:9000"}, func(string) error { return nil })
}

// A failure without an I/O cause keeps its error as it is.
func TestBindingReadWithoutCause(t *testing.T) {
	if bindingReadFailure(nil) || markBindingRead(ErrLaunchUnsupported, nil) != ErrLaunchUnsupported {
		t.Fatal("no cause is not a read failure")
	}
}
