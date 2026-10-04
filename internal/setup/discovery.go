package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"path/filepath"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

type Discovery struct {
	Request Request  `json:"request"`
	Units   []string `json:"units"`
	Pending bool     `json:"pending"`
}

func Discover(ctx context.Context, home string) (Discovery, error) {
	result := Discovery{Request: Request{Version: 1, Profile: Profile{Version: 1, StatePath: filepath.Join(home, ".local/state/gpu-workload-supervisor/state.db"), SystemctlPath: "/usr/bin/systemctl", NvidiaSMIPath: "/usr/bin/nvidia-smi"}, Catalog: control.Catalog{Version: 1}}}
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
		if data, err := privateRead(filepath.Join(root, "operator.json")); err == nil {
			if err := json.Unmarshal(data, &result.Request.Profile); err != nil {
				return result, err
			}
			snapshot, err := ReadCatalog(ctx, result.Request.Profile.StatePath)
			if err != nil {
				return result, err
			}
			result.Request.Catalog = snapshot.Catalog
			result.Request.ExpectedRevision = snapshot.Revision
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	output, err := runCommand(ctx, "/usr/bin/systemctl", "--user", "list-unit-files", "--type=service", "--no-legend", "--no-pager")
	if err != nil {
		return result, err
	}
	if len(output) > 1048576 {
		return result, errors.New("unit discovery output too large")
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasSuffix(fields[0], ".service") {
			result.Units = append(result.Units, fields[0])
		}
	}
	return result, nil
}
