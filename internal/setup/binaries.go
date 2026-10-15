package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

var binaries = []string{"gpu-mode", "gpu-workload-proxy", "gpu-operator", "gpu-setup"}

type binaryManifest struct {
	Release string            `json:"release"`
	Hashes  map[string]string `json:"hashes"`
}

func (b Backend) retainBinaries(root string) error {
	directory := filepath.Join(root, "activated-binaries")
	if err := mkdirTrusted(directory); err != nil {
		return err
	}
	manifest := binaryManifest{Release: deployment.Release, Hashes: map[string]string{}}
	for _, name := range binaries {
		path := filepath.Join(b.binaryDirectory, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Sys().(*syscall.Stat_t).Uid != b.packageBinaryUID {
			return errors.New("package executable is not trusted")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifest.Hashes[name] = digest(data)
		if err := deployment.AtomicWrite(filepath.Join(directory, name), data); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(directory, "manifest.json"), manifest)
}
func copyActivation(ctx context.Context, home, root, destination string, profile []byte, plan unitPlan) error {
	data, err := privateRead(filepath.Join(root, "activated-binaries/manifest.json"))
	if err != nil {
		return errors.New("prior activated binary set missing; repair before upgrade")
	}
	var manifest binaryManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	var old Profile
	if err := json.Unmarshal(profile, &old); err != nil {
		return err
	}
	if manifest.Release != old.ActivatedRelease {
		return errors.New("prior binary release differs from profile")
	}
	files := map[string][]byte{"operator.json": profile, "manifest.json": data}
	marker, err := privateRead(old.StatePath + deployment.Suffix)
	if err != nil {
		return err
	}
	files["state.db"+deployment.Suffix] = marker
	accepted, err := ReadCatalog(ctx, old.StatePath)
	if err != nil {
		return err
	}
	catalog, err := json.Marshal(accepted.Catalog)
	if err != nil {
		return err
	}
	files["catalog.json"] = catalog
	if err := collectActivationUnits(home, accepted.Catalog, plan, files); err != nil {
		return err
	}
	ownershipData, err := privateRead(filepath.Join(root, manifestName))
	if err != nil {
		return err
	}
	var owned ownership
	if err := json.Unmarshal(ownershipData, &owned); err != nil {
		return err
	}
	if owned.Version != 1 || owned.Files == nil {
		return errors.New("invalid prior ownership manifest")
	}
	owned.Files["operator.json"] = digest(profile)
	owned.Files["catalog.json"] = digest(catalog)
	ownershipData, err = json.Marshal(owned)
	if err != nil {
		return err
	}
	files[manifestName] = ownershipData
	if err := collectActivatedBinaries(root, manifest, files); err != nil {
		return err
	}
	return writeActivationBackup(destination, files)
}

type ownedUnitBackupManifest struct {
	Version int               `json:"version"`
	Hashes  map[string]string `json:"hashes"`
}

// collectActivationUnits retains the accepted catalog's exact unit bytes. Writes
// have already run for runtime verification, so their verified pre-write snapshots
// are authoritative; the current rendering would belong to the new activation.
func collectActivationUnits(home string, catalog control.Catalog, plan unitPlan, files map[string][]byte) error {
	entries, err := acceptedOwnedUnitEntries(catalog, home)
	if err != nil {
		return err
	}
	manifest := ownedUnitBackupManifest{Version: 1, Hashes: map[string]string{}}
	for _, entry := range entries {
		data, saved := plan.prior[entry.unit]
		if !saved {
			data, err = privateRead(entry.path)
			if err != nil {
				return err
			}
		}
		if digest(data) != entry.proof {
			return fmt.Errorf("prior owned unit checksum mismatch: %s", entry.unit)
		}
		manifest.Hashes[entry.unit] = entry.proof
		files[filepath.Join("owned-units", entry.unit)] = data
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	files["owned-units.json"] = data
	return nil
}

func collectActivatedBinaries(root string, manifest binaryManifest, files map[string][]byte) error {
	for _, name := range binaries {
		binary, err := privateRead(filepath.Join(root, "activated-binaries", name))
		if err != nil {
			return err
		}
		if digest(binary) != manifest.Hashes[name] {
			return errors.New("prior activated binary checksum mismatch")
		}
		files[name] = binary
	}
	return nil
}

func writeActivationBackup(destination string, files map[string][]byte) error {
	for name, data := range files {
		path := filepath.Join(destination, name)
		if filepath.Dir(name) != "." {
			if err := mkdirTrusted(filepath.Dir(path)); err != nil {
				return err
			}
		}
		if previous, err := privateRead(path); err == nil {
			if digest(previous) != digest(data) {
				return errors.New("backup tuple already exists with different contents")
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := deployment.AtomicWrite(path, data); err != nil {
			return err
		}
	}
	// AtomicWrite syncs each file's immediate directory. Also persist the
	// owned-units directory entry in the tuple root before originals can be
	// removed by the successful activation.
	return syncDirectory(destination)
}
