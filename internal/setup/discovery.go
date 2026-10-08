package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"path/filepath"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

type Discovery struct {
	Errors       []string               `json:"errors,omitempty"`
	Request      Request                `json:"request"`
	Units        []string               `json:"units"`
	Pending      bool                   `json:"pending"`
	Applications []ApplicationCandidate `json:"applications"`
	OwnedUnits   []OwnedUnitStatus      `json:"ownedUnits"`
}

// OwnedUnitStatus reports one supervisor-owned unit file so the UI can offer
// adopt/cleanup choices. State is "managed", "orphaned", or "modified".
type OwnedUnitStatus struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	State  string `json:"state"`
}

func Discover(ctx context.Context, home string) (Discovery, error) {
	return SystemBackend().Discover(ctx, home)
}

func (b Backend) Discover(ctx context.Context, home string) (Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result := Discovery{Request: Request{Version: 1, Profile: Profile{Version: 1, StatePath: deployment.DefaultStatePath(), SystemctlPath: "/usr/bin/systemctl", NvidiaSMIPath: "/usr/bin/nvidia-smi"}, Catalog: control.Catalog{Version: 1}}}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if data, err := privateRead(filepath.Join(root, "activation.json")); err == nil {
		// A pending activation is resumed verbatim; changing it could discard the
		// only plan capable of completing a catalog commit after interruption.
		var pending activation
		if err := json.Unmarshal(data, &pending); err != nil {
			return result, err
		}
		marker, err := deployment.Read(pending.Request.Profile.StatePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		if marker.Maintenance {
			result.Request = pending.Request
			result.Pending = true
		}
	}
	if !result.Pending {
		if err := discoverCurrent(ctx, root, &result); err != nil {
			return result, err
		}
	}
	// Unit listing is best-effort: endpoint probes and saved state still apply
	// when the user bus is unavailable or its answer is unusable.
	var units []string
	if output, err := b.runCommand(ctx, "/usr/bin/systemctl", "--user", "list-unit-files", "--type=service", "--no-legend", "--no-pager"); err == nil && len(output) <= 1048576 {
		units = serviceUnits(output)
	} else {
		result.Errors = append(result.Errors, "Application services could not be listed. Check your desktop user session and retry.")
	}
	result.Units = []string{}
	b.discoverApplications(ctx, &result, units)
	owned, err := discoverOwnedUnits(home, result.Request.Catalog)
	if err != nil {
		return result, err
	}
	result.OwnedUnits = owned
	return result, nil
}

// ErrOwnedUnitDiscovery marks an owned-unit directory that exists but cannot
// be inspected; runtime preflight propagates the same failure, so discovery
// must fail loudly instead of reporting an empty inventory.
var ErrOwnedUnitDiscovery = errors.New("owned unit directory unreadable")

// discoverOwnedUnits inventories supervisor-owned unit files by directory
// listing and content digest only; it never parses unit contents.
func discoverOwnedUnits(home string, catalog control.Catalog) ([]OwnedUnitStatus, error) {
	entries, err := os.ReadDir(ownedUnitDirectory(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOwnedUnitDiscovery, err)
	}
	managed := map[string]string{}
	// Exact adopted bindings (owned profiles converted to adopted while keeping
	// the file) account for the unit, matching preflightOwned semantics.
	adopted := map[string]string{}
	for _, p := range catalog.Profiles {
		if p.NativeModel == nil {
			continue
		}
		if p.NativeModel.Owned != nil {
			managed[p.Unit] = p.NativeModel.LaunchSHA256
			continue
		}
		if p.AdoptedOwnedFile() {
			adopted[p.NativeModel.LaunchFile] = p.NativeModel.LaunchSHA256
		}
	}
	var owned []OwnedUnitStatus
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "gws-owned-") || !strings.HasSuffix(name, ".service") {
			continue
		}
		data, err := privateRead(filepath.Join(ownedUnitDirectory(home), name))
		if err != nil {
			// An unreadable or untrusted unit still blocks runtime preflight by
			// name; surface it so the UI can diagnose and clean up.
			owned = append(owned, OwnedUnitStatus{Name: name, State: "modified"})
			continue
		}
		status := discoveredOwnedUnitStatus(home, name, data, managed, adopted)
		owned = append(owned, status)
	}
	return owned, nil
}

func discoverCurrent(ctx context.Context, root string, result *Discovery) error {
	if data, err := privateRead(filepath.Join(root, "operator.json")); err == nil {
		if err := json.Unmarshal(data, &result.Request.Profile); err != nil {
			return err
		}
		snapshot, err := ReadCatalog(ctx, result.Request.Profile.StatePath)
		if err != nil {
			return err
		}
		result.Request.Catalog = snapshot.Catalog
		result.Request.ExpectedRevision = snapshot.Revision
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func serviceUnits(output []byte) []string {
	var units []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasSuffix(fields[0], ".service") {
			units = append(units, fields[0])
		}
	}
	return units
}

func discoveredOwnedUnitStatus(home, name string, data []byte, managed, adopted map[string]string) OwnedUnitStatus {
	status := OwnedUnitStatus{Name: name, Digest: digest(data), State: "orphaned"}
	if want, ok := managed[name]; ok {
		status.State = "managed"
		if want != status.Digest {
			status.State = "modified"
		}
	} else if want, ok := adopted[filepath.Join(ownedUnitDirectory(home), name)]; ok && want == status.Digest {
		status.State = "managed"
	}
	return status
}
