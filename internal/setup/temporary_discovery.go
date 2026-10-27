package setup

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type TemporaryCleanupRequest struct {
	ID                    string `json:"id"`
	Token                 string `json:"token"`
	ExternalControlPaused bool   `json:"externalControlPaused"`
}

func temporaryCleanupProfile(home string) (Profile, error) {
	var p Profile
	raw, err := privateRead(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"))
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

func checkNoTemporaryDiscovery(ctx context.Context, path string) error {
	db, err := readOnlyDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='temporary_discovery_sessions')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var pending bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM temporary_discovery_sessions WHERE status!='completed')`).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return store.ErrTemporaryCleanupRequired
	}
	return nil
}

func TemporaryStatus(ctx context.Context, home string) (*control.TemporaryDiscoverySession, error) {
	p, err := temporaryCleanupProfile(home)
	if err != nil {
		return nil, err
	}
	db, err := readOnlyDB(p.StatePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return store.InspectTemporaryDiscoveryStatus(ctx, db)
}

func TemporaryCleanup(ctx context.Context, home string, r TemporaryCleanupRequest) error {
	return SystemBackend().TemporaryCleanup(ctx, home, r)
}

func (b Backend) TemporaryCleanup(ctx context.Context, home string, r TemporaryCleanupRequest) error {
	if !r.ExternalControlPaused || r.ID == "" || r.Token == "" {
		return errors.New("session identity and paused external application control are required for cleanup")
	}
	p, err := temporaryCleanupProfile(home)
	if err != nil {
		return err
	}
	proxy, err := lock.TryAcquire(p.StatePath + ".proxy.lock")
	if err != nil {
		return err
	}
	defer proxy.Close()
	gate, err := lock.TryAcquire(p.StatePath + ".lock")
	if err != nil {
		return err
	}
	defer gate.Close()
	if err := checkTemporaryCleanupDeployment(p); err != nil {
		return err
	}
	if session, err := TemporaryStatus(ctx, home); err != nil {
		return err
	} else if session == nil {
		return errors.New("no pending temporary discovery session")
	}
	state, err := store.Open(ctx, p.StatePath)
	if err != nil {
		return err
	}
	defer state.Close()
	execution, err := state.AcquireUserExecution(ctx, false)
	if err != nil {
		return err
	}
	defer execution.Close()
	return b.finishTemporaryCleanup(ctx, p, state, r)
}

func (b Backend) finishTemporaryCleanup(ctx context.Context, p Profile, state *store.Store, r TemporaryCleanupRequest) error {
	if err := state.CheckTemporaryDiscovery(ctx, r.ID, r.Token); err != nil {
		return err
	}
	session, err := state.TemporaryDiscoveryStatus(ctx)
	if err != nil {
		return err
	}
	if session == nil || session.ID != r.ID {
		return store.ErrStaleFence
	}
	snapshot, err := state.Catalog(ctx)
	if err != nil {
		return err
	}
	manager, err := b.makeRuntime(Request{Profile: p, Catalog: removalObservationCatalog(snapshot.Catalog)})
	if err != nil {
		return err
	}
	cleanup, ok := manager.(interface {
		StopTemporaryDiscovery(context.Context, control.TemporaryDiscoveryCandidate, string) error
	})
	if !ok {
		return errors.New("runtime cannot clean up the recorded temporary invocation")
	}
	invocation := session.InvocationID
	if invocation == "" {
		invocation = session.LaunchEvidence.InvocationID
	}
	if invocation != "" && (invocation != session.LaunchEvidence.InvocationID || session.LaunchEvidence.JobID == "" || session.LaunchEvidence.ActivationTimestamp == "") {
		return store.ErrStaleFence
	}
	if err := cleanup.StopTemporaryDiscovery(ctx, session.Candidate, invocation); err != nil {
		return err
	}
	if err := manager.ReleasedFor(ctx, control.WorkloadIdle); err != nil {
		return err
	}
	_, err = state.FinishTemporaryDiscovery(ctx, r.ID, r.Token)
	return err
}

func checkTemporaryCleanupDeployment(p Profile) error {
	marker, err := deployment.Read(p.StatePath)
	if err != nil {
		return err
	}
	if marker.Maintenance || p.ActivatedRelease == "" || marker.Release != p.ActivatedRelease {
		return errors.New("temporary cleanup requires a consistent activated deployment outside maintenance")
	}
	if marker.Release != deployment.Release && !newer(deployment.Release, marker.Release) {
		return errors.New("temporary cleanup cannot cross an unsupported release change")
	}
	file, err := deployment.OpenPrivate(p.StatePath)
	if err != nil {
		return err
	}
	return file.Close()
}
