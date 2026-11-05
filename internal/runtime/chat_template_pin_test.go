package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
)

// TestChatTemplatePinMatchesTheBinding checks that the model check pin of a
// template text equals the binding digest of a --chat-template file that
// holds the same text.
func TestChatTemplatePinMatchesTheBinding(t *testing.T) {
	for _, text := range []string{"{{ messages }}", "{%- for m in messages %}{{ '<|im_start|>' + m.role + '\n' + m.content }}{%- endfor %}\n", "{{ 'é中' }}"} {
		file := filepath.Join(t.TempDir(), "chat_template.jinja")
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		digest, err := ChatTemplateSHA256(file)
		if err != nil {
			t.Fatal(err)
		}
		if digest != modelcheck.PinTemplate(text) {
			t.Fatalf("binding digest %s differs from the model check pin %s", digest, modelcheck.PinTemplate(text))
		}
	}
}
