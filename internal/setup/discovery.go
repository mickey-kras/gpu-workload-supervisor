package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"path/filepath"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

type Discovery struct {
	Request      Request                `json:"request"`
	Units        []string               `json:"units"`
	Pending      bool                   `json:"pending"`
	Applications []ApplicationCandidate `json:"applications"`
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
	output, err := b.runCommand(ctx, "/usr/bin/systemctl", "--user", "list-unit-files", "--type=service", "--no-legend", "--no-pager")
	if err != nil {
		return result, err
	}
	if len(output) > 1048576 {
		return result, errors.New("unit discovery output too large")
	}
	result.Units = []string{}
	b.discoverApplications(ctx, &result, serviceUnits(output))
	return result, nil
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
