package setup

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

// MaxModelCheckRequestBytes bounds the JSON of a model check request
// (4 KiB).
const MaxModelCheckRequestBytes = 4096

// ModelCheckRequest asks setup to run the model checks of one accepted
// LLM workload.
type ModelCheckRequest struct {
	Version  int    `json:"version"`
	Workload string `json:"workload"`
}

// Errors of the model check actions. They hold no path.
var (
	ErrModelCheckRequest  = errors.New("the model check request is not valid")
	ErrModelNotConfigured = errors.New("the workload is not in the accepted configuration")
	ErrModelNotActivated  = errors.New("model checks need an activated setup; finish setup first")
	ErrModelNotLLM        = errors.New("model checks apply only to LLM workloads")
	ErrModelNoDirectory   = errors.New("the workload names no model folder, so setup cannot check its files")
	ErrResultNotCurrent   = errors.New("the stored result does not apply to the accepted workload; run the checks again")
	// ErrStateBusy reports that another setup change held the state lock
	// until the action timed out. Nothing was stored.
	ErrStateBusy = errors.New("setup was busy with another change until the time limit, so nothing was stored; try again")
)

// DecodeModelCheckRequest reads one strict model check request of at most
// MaxModelCheckRequestBytes and validates it.
func DecodeModelCheckRequest(r io.Reader) (ModelCheckRequest, error) {
	var request ModelCheckRequest
	if err := strictjson.DecodeLimited(r, MaxModelCheckRequestBytes, &request); err != nil {
		return request, err
	}
	if request.Version != 1 || !control.ValidWorkloadID(control.Workload(request.Workload)) {
		return request, ErrModelCheckRequest
	}
	return request, nil
}

// checkTarget is an accepted LLM workload with a model directory.
type checkTarget struct {
	statePath, release string
	profile            control.WorkloadProfile
	root, pin          string
}

// sameLaunch reports whether two targets name the same state, release, model
// directory and chat template pin.
func (t checkTarget) sameLaunch(other checkTarget) bool {
	return t.statePath == other.statePath && t.release == other.release && t.root == other.root && t.pin == other.pin
}

// loadCheckTarget finds an accepted LLM workload that has a model
// directory. It refuses an image or unidentified workload and an LLM
// workload that names only a model ID, such as an adopted service.
func loadCheckTarget(ctx context.Context, home, workload string) (checkTarget, error) {
	profile, found, err := readOperatorProfile(home)
	if err != nil {
		return checkTarget{}, err
	}
	if !found {
		return checkTarget{}, ErrModelNotConfigured
	}
	snapshot, err := ReadCatalog(ctx, profile.StatePath)
	if err != nil {
		return checkTarget{}, err
	}
	p, ok := snapshot.Catalog.Profile(control.Workload(workload))
	switch {
	case !ok:
		return checkTarget{}, ErrModelNotConfigured
	case p.NativeModel == nil:
		return checkTarget{}, ErrModelNotLLM
	case p.NativeModel.Owned == nil || p.NativeModel.Owned.ModelPath == "":
		return checkTarget{}, ErrModelNoDirectory
	}
	return checkTarget{statePath: profile.StatePath, release: profile.ActivatedRelease, profile: p, root: p.NativeModel.Owned.ModelPath, pin: p.NativeModel.ChatTemplateSHA256}, nil
}

// loadWriteTarget is loadCheckTarget for an action that writes: setup must
// be activated, and the deployment must not be in maintenance or carry
// another release.
func loadWriteTarget(ctx context.Context, home, workload string) (checkTarget, error) {
	target, err := loadCheckTarget(ctx, home, workload)
	if err != nil {
		return target, err
	}
	if target.release == "" {
		return target, ErrModelNotActivated
	}
	return target, deployment.Check(target.statePath, target.release)
}

// gatePoll bounds the wait between two tries for the state gate. The wait
// starts at 10 ms and doubles up to this bound.
const gatePoll = time.Second

// acquireGate waits for the state gate until the action context ends, so a
// finished run waits as long as its timeout allows. A timeout during the
// wait means another change held the gate and returns ErrStateBusy; a cancel
// returns modelcheck.ErrCheckCanceled. A context that ended before the wait
// returns the run's stop error (see modelcheck.Stopped). Other errors pass
// through.
func acquireGate(ctx context.Context, statePath string) (*lock.File, error) {
	if err := modelcheck.Stopped(ctx); err != nil {
		return nil, err
	}
	delay := 10 * time.Millisecond
	for {
		gate, err := lock.TryAcquire(statePath + ".lock")
		if err == nil {
			return gate, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrStateBusy
			}
			return nil, modelcheck.ErrCheckCanceled
		case <-timer.C:
		}
		delay = min(2*delay, gatePoll)
	}
}

// writeTarget runs write with the writable store, as the other setup
// write paths do. It holds the state gate, so no apply or activation runs
// during the write. Under the gate it checks the deployment again, and
// reads the target again: a workload whose state path, release, model
// directory or chat template pin changed after target was read gives
// ErrResultNotCurrent, and nothing is written.
func writeTarget(ctx context.Context, home string, target checkTarget, write func(*store.Store, checkTarget) (ModelStatusEntry, error)) (ModelStatusEntry, error) {
	gate, err := acquireGate(ctx, target.statePath)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	defer gate.Close()
	fresh, err := loadWriteTarget(ctx, home, string(target.profile.ID))
	if err != nil {
		return ModelStatusEntry{}, err
	}
	if !target.sameLaunch(fresh) {
		return ModelStatusEntry{}, ErrResultNotCurrent
	}
	st, err := store.Open(ctx, fresh.statePath)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	defer st.Close()
	return write(st, fresh)
}

// ModelCheck runs the checks of one accepted LLM workload and stores the
// result, keyed by the workload ID. The checks read only the local model
// files. A run that is canceled, stops at its time limit, or whose checker
// cannot make a record, stores nothing. Any other run replaces the stored
// result, also with findings or an error. The entry describes the stored
// result.
func ModelCheck(ctx context.Context, home string, request ModelCheckRequest) (ModelStatusEntry, error) {
	target, err := loadWriteTarget(ctx, home, request.Workload)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	checker := modelcheck.Checker{Version: modelcheck.CheckerVersion}
	record, err := checker.Run(ctx, modelcheck.Request{Model: request.Workload, Root: target.root})
	if err != nil {
		return ModelStatusEntry{}, err
	}
	return writeTarget(ctx, home, target, func(st *store.Store, fresh checkTarget) (ModelStatusEntry, error) {
		if err := st.PutModelCheck(ctx, record); err != nil {
			return ModelStatusEntry{}, err
		}
		return storedEntry(ctx, st, fresh)
	})
}

// storedEntry reads the stored result of the target again.
func storedEntry(ctx context.Context, st *store.Store, target checkTarget) (ModelStatusEntry, error) {
	record, err := st.ModelCheck(ctx, string(target.profile.ID))
	if err != nil {
		return ModelStatusEntry{}, err
	}
	return recordEntry(target, record), nil
}

// recordEntry returns the status entry of record for target.
func recordEntry(target checkTarget, record modelcheck.Record) ModelStatusEntry {
	entry, _, _ := baseEntry(target.profile)
	entry.Status, entry.Reason = modelcheck.StoredStatus(record, string(target.profile.ID), target.root, target.pin)
	describeRecord(&entry, record, target.root, target.pin)
	return entry
}
