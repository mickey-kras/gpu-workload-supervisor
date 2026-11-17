package setup

import (
	"context"
	"testing"
)

func TestModelStatusWithoutProfileReturnsEmptyList(t *testing.T) {
	report, err := ModelStatus(context.Background(), t.TempDir(), ModelStatusRequest{Version: 1})
	if err != nil || report.Version != 1 || len(report.Models) != 0 {
		t.Fatalf("ModelStatus = %+v, %v", report, err)
	}
	if _, err := ModelStatus(context.Background(), t.TempDir(), ModelStatusRequest{Version: 2}); err == nil {
		t.Fatal("expected invalid version error")
	}
}
