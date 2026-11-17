package setup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

// MaxModelCheckRequestBytes bounds the JSON of a model check request (4 KiB).
const MaxModelCheckRequestBytes = 4096

// ModelCheckRequest asks setup to run local safetensors/template checks on an accepted LLM workload.
type ModelCheckRequest struct {
	Version  int               `json:"version"`
	Workload string            `json:"workload"`
	Source   modelcheck.Source `json:"source"`
}

var (
	ErrModelCheckRequest  = errors.New("the model check request is not valid")
	ErrModelNotConfigured = errors.New("the workload is not in the accepted configuration")
	ErrModelNotActivated  = errors.New("model checks need an activated setup; finish setup first")
	ErrModelNotLLM        = errors.New("model checks apply only to LLM workloads")
	ErrModelNoDirectory   = errors.New("the workload names no model folder, so setup cannot check its files")
	ErrResultNotCurrent   = errors.New("the stored result does not apply to the accepted workload; run the checks again")
	ErrStateBusy          = errors.New("setup was busy with another change until the time limit, so nothing was stored; try again")
)

func (r ModelCheckRequest) valid() bool {
	return r.Version == 1 && control.ValidWorkloadID(control.Workload(r.Workload)) && r.Source.Valid()
}

// DecodeModelCheckRequest reads and validates one strict model check request.
func DecodeModelCheckRequest(r io.Reader) (ModelCheckRequest, error) {
	var request ModelCheckRequest
	if err := strictjson.DecodeLimited(r, MaxModelCheckRequestBytes, &request); err != nil {
		return request, err
	}
	if !request.valid() {
		return request, ErrModelCheckRequest
	}
	return request, nil
}

type checkTarget struct {
	statePath, release string
	profile            control.WorkloadProfile
	root, pin          string
}

func (t checkTarget) sameLaunch(other checkTarget) bool {
	return t.statePath == other.statePath && t.release == other.release && t.root == other.root && t.pin == other.pin
}

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
	return checkTarget{
		statePath: profile.StatePath,
		release:   profile.ActivatedRelease,
		profile:   p,
		root:      p.NativeModel.Owned.ModelPath,
		pin:       p.NativeModel.ChatTemplateSHA256,
	}, nil
}

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

const gatePoll = time.Second

func acquireCheckGate(ctx context.Context, statePath string) (*lock.File, error) {
	if ctx.Err() != nil {
		return nil, modelcheck.Stopped(ctx)
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

// ModelCheck runs local safetensors and chat-template checks on one accepted LLM workload and persists the record.
func ModelCheck(ctx context.Context, home string, request ModelCheckRequest, client *http.Client) (ModelStatusEntry, error) {
	target, err := loadWriteTarget(ctx, home, request.Workload)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	checker := modelcheck.Checker{Client: client, Hub: modelcheck.DefaultHub, Version: modelcheck.CheckerVersion}
	record, err := checker.Run(ctx, modelcheck.Request{Model: request.Workload, Root: target.root, Source: request.Source})
	if err != nil {
		return ModelStatusEntry{}, err
	}
	gate, err := acquireCheckGate(ctx, target.statePath)
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
	if err := st.PutModelCheck(ctx, record); err != nil {
		return ModelStatusEntry{}, err
	}
	stored, err := st.ModelCheck(ctx, string(fresh.profile.ID))
	if err != nil {
		return ModelStatusEntry{}, err
	}
	entry, _, _ := baseEntry(fresh.profile)
	entry.Status, entry.Reason = modelcheck.StoredStatus(stored, string(fresh.profile.ID), fresh.root, fresh.pin)
	describeRecord(&entry, stored, fresh.root, fresh.pin)
	return entry, nil
}
