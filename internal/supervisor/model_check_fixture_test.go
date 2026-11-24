package supervisor

import (
	"context"
	"testing"

	"github.com/mickey-kras/llm-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/llm-workload-supervisor/internal/store"
)

// allowCheckedModels stores a passing model check result for each vLLM
// workload of c and skips the read of the model directory. Tests that are
// not about the launch gate use it; model_gate_test.go covers the gate.
func allowCheckedModels(t *testing.T, c *Controller, s *store.Store) {
	t.Helper()
	for _, p := range c.config.Catalog.Catalog.Profiles {
		if p.NativeModel == nil || p.NativeModel.ChatTemplateSHA256 == "" {
			continue
		}
		record := gateRecord()
		record.Model = string(p.ID)
		if p.NativeModel.Owned != nil {
			record.Root = p.NativeModel.Owned.ModelPath
		}
		record.ChatTemplateSHA256 = p.NativeModel.ChatTemplateSHA256
		if err := s.PutModelCheck(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	c.modelFilesUnchanged = func(modelcheck.Record) error { return nil }
}
