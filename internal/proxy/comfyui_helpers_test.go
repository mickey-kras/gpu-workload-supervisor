package proxy

import "context"

// reconcile runs one bounded pass and ignores the per-attempt diagnostics.
func (a *comfyUIAdapter) reconcile(ctx context.Context) error {
	return a.reconcileWithDiagnostics(ctx, func(string, string) {
		// Tests check the returned errors only.
	})
}
