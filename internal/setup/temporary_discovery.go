package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/operator"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

type TemporaryDiscoveryRequest struct {
	Unit                  string            `json:"unit"`
	Expected              operator.Expected `json:"expected"`
	Consent               bool              `json:"consent"`
	ExternalControlPaused bool              `json:"externalControlPaused"`
}
type TemporaryCleanupRequest struct {
	ID                    string `json:"id"`
	Token                 string `json:"token"`
	ExternalControlPaused bool   `json:"externalControlPaused"`
}
type TemporaryDiscoveryResult struct {
	Session *control.TemporaryDiscoverySession `json:"session,omitempty"`
	Models  []ModelCandidate                   `json:"models,omitempty"`
	Error   string                             `json:"error,omitempty"`
}
type TemporaryDiscoveryStatus struct {
	Session   *control.TemporaryDiscoverySession `json:"session,omitempty"`
	Expected  *operator.Expected                 `json:"expected,omitempty"`
	Available bool                               `json:"available"`
	Reason    string                             `json:"reason,omitempty"`
}

func TemporaryStatus(ctx context.Context, home string) (TemporaryDiscoveryStatus, error) {
	return SystemBackend().TemporaryStatus(ctx, home)
}
func (b Backend) TemporaryStatus(ctx context.Context, home string) (TemporaryDiscoveryStatus, error) {
	result := TemporaryDiscoveryStatus{}
	data, err := privateRead(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"))
	if errors.Is(err, os.ErrNotExist) {
		result.Reason = "Temporary discovery requires an initialized compatible supervisor in healthy, closed Idle. You can continue with read-only discovery or enter the installed model manually."
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var profile Profile
	if err = json.Unmarshal(data, &profile); err != nil {
		return result, err
	}
	if err = profile.Validate(); err != nil {
		return result, err
	}
	inspection, err := inspectTemporaryDiscovery(ctx, profile)
	if errors.Is(err, store.ErrTemporaryDiscoveryUpgradeRequired) {
		result.Reason = err.Error()
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Session = inspection.Session
	state := inspection.State
	result.Expected = &operator.Expected{Incarnation: state.LeaseFence.Incarnation, Version: strconv.FormatUint(state.Version, 10), Owner: state.Owner, ConfigurationRevision: inspection.Catalog.Revision}
	if inspection.Eligibility != nil {
		result.Reason = inspection.Eligibility.Error()
	} else {
		result.Available = true
	}
	return result, nil
}

// temporaryController uses the authoritative activated deployment profile and
// both ordinary configuration and proxy lifetime gates. No setup plan, request
// profile, or uncommitted candidate can replace the runtime catalog.
func (b Backend) temporaryController(ctx context.Context, home string) (*supervisor.Controller, func(), error) {
	load := b.temporaryProfile
	if load == nil {
		load = operator.LoadProfile
	}
	p, err := load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, errors.New("temporary discovery requires an initialized compatible supervisor; continue read-only discovery or enter the model manually")
		}
		return nil, nil, fmt.Errorf("load temporary discovery deployment profile: %w", err)
	}
	if err = p.Validate(); err != nil {
		return nil, nil, err
	}
	proxy, err := lock.TryAcquire(p.StatePath + ".proxy.lock")
	if err != nil {
		return nil, nil, err
	}
	if _, err := inspectTemporaryDiscovery(ctx, p); err != nil {
		proxy.Close()
		return nil, nil, err
	}
	c, cleanup, err := b.activatedController(ctx, home)
	if err != nil {
		proxy.Close()
		return nil, nil, err
	}
	return c, func() { cleanup(); proxy.Close() }, nil
}
func TemporaryDiscover(ctx context.Context, home string, r TemporaryDiscoveryRequest) (TemporaryDiscoveryResult, error) {
	return SystemBackend().TemporaryDiscover(ctx, home, r)
}
func (b Backend) TemporaryDiscover(ctx context.Context, home string, r TemporaryDiscoveryRequest) (TemporaryDiscoveryResult, error) {
	result := TemporaryDiscoveryResult{}
	if !r.Consent || !r.ExternalControlPaused {
		return result, errors.New("explicit temporary-start consent and paused external application control are required")
	}
	if !selectedUnitName.MatchString(r.Unit) {
		return result, errors.New("select a recognized stopped Ollama installation")
	}
	version, err := strconv.ParseUint(r.Expected.Version, 10, 64)
	if err != nil || version == 0 || strconv.FormatUint(version, 10) != r.Expected.Version {
		return result, errors.New("invalid temporary discovery expected version")
	}
	c, cleanup, err := b.temporaryController(ctx, home)
	if err != nil {
		return result, err
	}
	defer cleanup()
	profile, err := b.automaticConfiguration(ctx, "ollama", r.Unit, "selection-pending")
	if err != nil {
		return result, err
	}
	if profile.NativeModel == nil || profile.NativeModel.Runtime != "ollama" {
		return result, errors.New("temporary discovery supports only qualified Ollama serve installations")
	}
	out, err := b.runCommand(ctx, "/usr/bin/systemctl", "--version")
	if err != nil {
		return result, err
	}
	systemdVersion, err := gpuruntime.SupportedSystemdPlacementVersion(out)
	if err != nil {
		return result, err
	}
	n := profile.NativeModel
	v := control.TemporaryDiscoveryCandidate{Unit: r.Unit, LaunchFile: n.LaunchFile, LaunchSHA256: n.LaunchSHA256, DropIns: n.DropIns, Endpoint: n.Endpoint, Cgroup: profile.Cgroup, SystemdSlice: "app.slice", SystemdVersion: systemdVersion}
	e := control.OperatorPrecondition{Incarnation: r.Expected.Incarnation, Version: version, Owner: r.Expected.Owner, ConfigurationRevision: r.Expected.ConfigurationRevision}
	raw, operationErr := c.DiscoverNativeTemporary(ctx, v, e, true, b.temporaryModels)
	statusCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result.Session, err = c.TemporaryDiscoveryStatus(statusCtx)
	if operationErr != nil {
		result.Error = operationErr.Error()
		return result, err
	}
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(raw, &result.Models); err != nil {
		return result, err
	}
	return result, nil
}

func (b Backend) temporaryModels(ctx context.Context, endpoint string) ([]byte, error) {
	// The existing read-only inventory probe makes only metadata requests.
	var found ApplicationCandidate
	var err error
	for {
		found, err = b.probeApplication(ctx, ProbeRequest{App: "ollama", Endpoint: endpoint})
		if err == nil && found.InventoryStatus == "available" {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return json.Marshal(found.Models)
}

func TemporaryCleanup(ctx context.Context, home string, r TemporaryCleanupRequest) (TemporaryDiscoveryResult, error) {
	return SystemBackend().TemporaryCleanup(ctx, home, r)
}
func (b Backend) TemporaryCleanup(ctx context.Context, home string, r TemporaryCleanupRequest) (TemporaryDiscoveryResult, error) {
	result := TemporaryDiscoveryResult{}
	if !r.ExternalControlPaused || r.ID == "" || r.Token == "" {
		return result, errors.New("session identity and paused external application control are required for cleanup")
	}
	c, cleanup, err := b.temporaryController(ctx, home)
	if err != nil {
		return result, err
	}
	defer cleanup()
	operationErr := c.CleanupTemporaryDiscovery(ctx, r.ID, r.Token)
	statusCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result.Session, err = c.TemporaryDiscoveryStatus(statusCtx)
	if operationErr != nil {
		result.Error = operationErr.Error()
	}
	return result, err
}

// Older compatible databases have no session table. Inspection stays read-only
// until setup's backup and quiescence checks authorize a schema upgrade.
func checkNoTemporaryDiscovery(ctx context.Context, path string) error {
	db, err := readOnlyDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var exists bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='temporary_discovery_sessions')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var pending bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM temporary_discovery_sessions WHERE status!='completed')`).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return store.ErrTemporaryCleanupRequired
	}
	return nil
}

// This preflight is read-only even for lifecycle verbs. Writable Store.Open is
// permitted only after the normal activation path has installed this schema.
func inspectTemporaryDiscovery(ctx context.Context, profile Profile) (store.TemporaryDiscoveryInspection, error) {
	if err := deployment.Check(profile.StatePath, profile.ActivatedRelease); err != nil {
		return store.TemporaryDiscoveryInspection{}, err
	}
	db, err := readOnlyDB(profile.StatePath)
	if err != nil {
		return store.TemporaryDiscoveryInspection{}, err
	}
	defer db.Close()
	return store.InspectTemporaryDiscovery(ctx, db)
}
