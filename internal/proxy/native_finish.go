package proxy

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

const (
	defaultNativeCompletionLimit = 128
	observedFinishTimeout        = 10 * time.Second
	observedFinishAttemptTimeout = 3 * time.Second
	observedFinishAttempts       = 3
	observedFinishBackoff        = 100 * time.Millisecond
)

// NativeCompletionFailure contains only fixed reason codes and attempt counts.
// It deliberately excludes storage errors and execution correlation metadata.
type NativeCompletionFailure struct {
	Reason   string
	Attempts int
}

// nativeFinishes owns reserved proof slots and persistence workers. A slot spans
// admission until either the response ends without proof or persistence ends.
// Streams still open after persistence remain bounded by Server's ordinary cap.
type nativeFinishes struct {
	mu       sync.Mutex
	limit    int
	reserved int
	workers  int
	sealed   bool
	drained  chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	report   func(NativeCompletionFailure)
}

func newNativeFinishes(limit int, report func(NativeCompletionFailure)) *nativeFinishes {
	if limit == 0 {
		limit = defaultNativeCompletionLimit
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &nativeFinishes{limit: limit, drained: make(chan struct{}), ctx: ctx, cancel: cancel, report: report}
}

// A reservation has exactly one owner: the HTTP producer until handoff, then
// the worker. All transitions use the owner's mutex, including shutdown sealing.
type nativeFinishReservation struct {
	owner       *nativeFinishes
	transferred bool
	released    bool
}

func (f *nativeFinishes) reserve() *nativeFinishReservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sealed || f.reserved >= f.limit {
		return nil
	}
	f.reserved++
	return &nativeFinishReservation{owner: f}
}

func (r *nativeFinishReservation) release() {
	if r == nil {
		return
	}
	f := r.owner
	f.mu.Lock()
	defer f.mu.Unlock()
	if !r.transferred && !r.released {
		r.released = true
		f.reserved--
	}
}

func (r *nativeFinishReservation) submit(h *Handler, requestID string, fence control.Fence, token string) {
	f := r.owner
	f.mu.Lock()
	if f.sealed || r.released || r.transferred {
		f.mu.Unlock()
		return
	}
	r.transferred = true
	f.workers++
	f.mu.Unlock()
	// Only immutable registration evidence crosses into persistence. No request
	// body, response body, or upstream transport is retained or replayed.
	go func() {
		defer f.finished()
		h.finishObserved(f.ctx, requestID, fence, token, f.report)
	}()
}

func (f *nativeFinishes) finished() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserved--
	f.workers--
	if f.sealed && f.workers == 0 {
		close(f.drained)
	}
}

// DrainNativeCompletions seals proof submission and waits for accepted writes.
// Call after draining HTTP producers and before closing the store/recovery lock.
// Deadline expiry cancels retries, then joins active writes before returning.
// SQLite's busy handler may outlive cancellation; this is not a hard wall-clock
// shutdown bound. Report callbacks must return promptly for the same reason.
// Calls may be repeated or concurrent. A late HTTP producer cannot submit work.
func (h *Handler) DrainNativeCompletions(ctx context.Context) error {
	f := h.nativeFinishes
	if f == nil {
		return nil
	}
	f.mu.Lock()
	if !f.sealed {
		f.sealed = true
		if f.workers == 0 {
			close(f.drained)
		}
	}
	f.mu.Unlock()
	defer f.cancel()
	select {
	case <-f.drained:
		return nil
	default:
	}
	select {
	case <-f.drained:
		return nil
	case <-ctx.Done():
		f.cancel()
		<-f.drained
		return ctx.Err()
	}
}

func (h *Handler) finishObserved(parent context.Context, requestID string, fence control.Fence, token string, report func(NativeCompletionFailure)) {
	ctx, cancel := context.WithTimeout(parent, observedFinishTimeout)
	defer cancel()
	for attempt := 1; attempt <= observedFinishAttempts; attempt++ {
		if ctx.Err() != nil {
			reportNativeFailure(report, "canceled", attempt-1)
			return
		}
		attemptCtx, stop := context.WithTimeout(ctx, observedFinishAttemptTimeout)
		err := h.store.FinishWorkToken(attemptCtx, requestID, h.workload, fence, token, store.WorkCompleted)
		stop()
		// A concurrent callback/reconciler or an uncertain commit may have
		// finished this exact tuple already. Never overwrite its terminal outcome.
		if err == nil || errors.Is(err, store.ErrWorkAlreadyCompleted) {
			return
		}
		reason := nativeFinishFailureReason(ctx, err, attempt)
		reportNativeFailure(report, reason, attempt)
		if reason != "retrying" {
			return
		}
		if !waitNativeRetry(ctx, time.Duration(attempt)*observedFinishBackoff) {
			reportNativeFailure(report, "canceled", attempt)
			return
		}
	}
}

func nativeFinishFailureReason(ctx context.Context, err error, attempt int) string {
	if ctx.Err() != nil {
		return "canceled"
	}
	if errors.Is(err, store.ErrStaleFence) || errors.Is(err, store.ErrWorkloadMismatch) || errors.Is(err, store.ErrRegistrationTokenMismatch) || errors.Is(err, sql.ErrNoRows) {
		return "rejected"
	}
	if attempt == observedFinishAttempts {
		return "exhausted"
	}
	return "retrying"
}

func reportNativeFailure(report func(NativeCompletionFailure), reason string, attempts int) {
	if report != nil {
		report(NativeCompletionFailure{Reason: reason, Attempts: attempts})
	}
}

func waitNativeRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
