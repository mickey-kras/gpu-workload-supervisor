package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/proxy"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	_ "modernc.org/sqlite"
)

func TestForwardingFailureNeedsVerifiedAuditedResolution(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	stateStore, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateStore.Close()
	runtime := &fakeRuntime{}
	controller := testController(t, stateStore, runtime)
	transitionNumber := 0
	controller.id = func() (string, error) {
		transitionNumber++
		return fmt.Sprintf("transition-%d", transitionNumber), nil
	}
	if _, err := controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Switch(ctx, control.WorkloadMedia, "test")
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := proxy.New(stateStore, proxy.Config{
		Upstream: upstream, Workload: control.WorkloadMedia,
		ExecutionRoutes: []proxy.Route{{Method: http.MethodPost, Path: "/execute"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/execute", nil)
	request.Header.Set(proxy.DefaultRequestIDHeader, "failed-forward")
	request.Header.Set(proxy.DefaultFenceIDHeader, state.LeaseFence.Incarnation)
	request.Header.Set(proxy.DefaultFenceEpochHeader, "2")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("forwarding status = %d", response.Code)
	}
	if response.Header().Get(proxy.DefaultRegistrationTokenHeader) != "" {
		t.Fatal("failed forwarding leaked a registration token to caller")
	}
	controller.config.DrainTimeout = 5 * time.Millisecond
	if _, err := controller.Switch(ctx, control.WorkloadText, "test"); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("initial switch error = %v", err)
	}
	if _, err := controller.Recover(ctx); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("recovery must not stop unresolved work: %v", err)
	}
	if _, err := controller.Switch(ctx, control.WorkloadText, "test"); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("switch after ordinary recovery error = %v", err)
	}

	proxyLock, err := lock.AcquireShared(dbPath + ".proxy.lock")
	if err != nil {
		t.Fatal(err)
	}
	if exclusive, err := lock.TryAcquire(dbPath + ".proxy.lock"); err == nil {
		exclusive.Close()
		t.Fatal("resolution lock acquired while proxy is active")
	}
	if err := proxyLock.Close(); err != nil {
		t.Fatal(err)
	}
	recoveryLock, err := lock.TryAcquire(dbPath + ".proxy.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer recoveryLock.Close()
	closed, count, err := controller.ResolveUnfinishedWork(ctx, " \tincident: upstream submission unknown\n")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || closed.Admission != control.AdmissionClosed || closed.LeaseFence == state.LeaseFence {
		t.Fatalf("resolution count=%d state=%#v", count, closed)
	}

	auditDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer auditDB.Close()
	var outcome, reason string
	var auditedCount int64
	if err := auditDB.QueryRowContext(ctx, `SELECT completion_outcome FROM registered_work WHERE request_id = ?`, "failed-forward").Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if err := auditDB.QueryRowContext(ctx, `SELECT reason, abandoned_work FROM work_resolutions ORDER BY sequence DESC LIMIT 1`).Scan(&reason, &auditedCount); err != nil {
		t.Fatal(err)
	}
	if outcome != "abandoned" || reason != "incident: upstream submission unknown" || auditedCount != 1 {
		t.Fatalf("outcome=%q audit reason=%q count=%d", outcome, reason, auditedCount)
	}
	if _, err := controller.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	final, err := controller.Switch(ctx, control.WorkloadText, "test")
	if err != nil {
		t.Fatal(err)
	}
	if final.ActiveWorkload != control.WorkloadText || final.Admission != control.AdmissionOpen {
		t.Fatalf("final state = %#v", final)
	}
}

func TestResolutionVerificationFailureKeepsWorkIncomplete(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &fakeRuntime{}
	controller := testController(t, stateStore, runtime)
	if _, err := controller.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Switch(ctx, control.WorkloadMedia, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.AdmitWorkToken(ctx, "pending", "", control.WorkloadMedia, state.LeaseFence); err != nil {
		t.Fatal(err)
	}
	runtime.stopErr = errors.New("stop failed")
	closed, count, err := controller.ResolveUnfinishedWork(ctx, "failed stop")
	if err == nil || count != 0 || closed.Admission != control.AdmissionClosed {
		t.Fatalf("failed resolution count=%d state=%#v error=%v", count, closed, err)
	}
	// Ordinary recovery cannot stop runtimes while registration completion
	// remains uncertain; explicit verified resolution is still required.
	runtime.stopErr = nil
	controller.config.DrainTimeout = 5 * time.Millisecond
	if _, err := controller.Recover(ctx); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("unresolved work no longer blocks recovery: %v", err)
	}
	if _, err := controller.Switch(ctx, control.WorkloadText, "test"); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("failed recovery no longer blocks switch: %v", err)
	}
}

func TestResolutionRejectsUserOwnershipWithoutStoppingRuntime(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText}
	controller := testController(t, stateStore, runtime)
	state, err := stateStore.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Owner = control.OwnerUser
	if _, err := stateStore.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.ResolveUnfinishedWork(ctx, ""); err == nil {
		t.Fatal("blank audit reason accepted")
	}
	if _, _, err := controller.ResolveUnfinishedWork(ctx, "incident-123"); !errors.Is(err, ErrUserOwned) {
		t.Fatalf("user-owned recovery error = %v", err)
	}
	if len(runtime.calls) != 0 {
		t.Fatalf("user runtime was stopped: %#v", runtime.calls)
	}
}

func TestResolutionRejectsInvalidReasonWithoutDisruptingWorkload(t *testing.T) {
	ctx := context.Background()
	stateStore := openStore(t)
	runtime := &fakeRuntime{active: control.WorkloadText}
	controller := testController(t, stateStore, runtime)
	before, err := controller.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{" \t\n ", strings.Repeat("x", 513)} {
		_, count, err := controller.ResolveUnfinishedWork(ctx, reason)
		if err == nil || count != 0 {
			t.Fatalf("invalid reason %q: count=%d err=%v", reason, count, err)
		}
		after, err := stateStore.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if after != before || runtime.active != control.WorkloadText || len(runtime.calls) != 0 {
			t.Fatalf("invalid reason changed workload: before=%#v after=%#v calls=%v", before, after, runtime.calls)
		}
	}
}
