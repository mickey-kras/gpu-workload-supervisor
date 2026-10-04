package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

var binaryDirectory = "/usr/bin"
var binaries = []string{"gpu-mode", "gpu-workload-proxy", "gpu-operator", "gpu-setup"}

type binaryManifest struct {
	Release string            `json:"release"`
	Hashes  map[string]string `json:"hashes"`
}

func retainBinaries(root string) error {
	directory := filepath.Join(root, "activated-binaries")
	if err := mkdirTrusted(directory); err != nil {
		return err
	}
	manifest := binaryManifest{Release: deployment.Release, Hashes: map[string]string{}}
	for _, name := range binaries {
		path := filepath.Join(binaryDirectory, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Sys().(*syscall.Stat_t).Uid != 0 {
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
func copyActivation(root, destination string, profile []byte) error {
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
	catalog, err := privateRead(filepath.Join(root, "catalog.json"))
	if err != nil {
		return err
	}
	files["catalog.json"] = catalog
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
	for name, data := range files {
		path := filepath.Join(destination, name)
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
	return nil
}
