package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"os"
	"time"
)

func validateAuditFlags(command, before string, batch int) (time.Time, error) {
	if command != "prune-audit" {
		return time.Time{}, nil
	}
	cutoff, err := time.Parse(time.RFC3339, before)
	if err != nil || !cutoff.Before(time.Now()) || batch < 1 || batch > 1024 {
		return time.Time{}, errors.New("prune-audit requires past -audit-before RFC3339 and -audit-batch 1..1024")
	}
	return cutoff, nil
}
func pruneAudit(ctx context.Context, stateStore *store.Store, cutoff time.Time, batch int) error {
	count, err := stateStore.PruneAuditHistory(ctx, cutoff, batch)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]int64{"prunedAuditRecords": count})
}
