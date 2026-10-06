// Package deployment fences managed desktop state before any migration or effect.
package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// Release is set identically for all binaries by the release build.
var Release = "dev"

const (
	Suffix    = ".deployment.json"
	stateFile = "state.db"
)

func DefaultStatePath() string {
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		return filepath.Join(root, "gpu-workload-supervisor", stateFile)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return stateFile
	}
	return filepath.Join(home, ".local", "state", "gpu-workload-supervisor", stateFile)
}

// Profile is the operator.json schema shared by setup (the writer) and the
// operator (the reader). capacityHeadroomMiB stays without omitempty to match
// the bytes setup has always written.
type Profile struct {
	Version                 int    `json:"version"`
	StatePath               string `json:"statePath"`
	ActivatedRelease        string `json:"activatedRelease"`
	SystemctlPath           string `json:"systemctlPath"`
	NvidiaSMIPath           string `json:"nvidiaSMIPath"`
	GPUIndex                int    `json:"gpuIndex"`
	CapacityHeadroomMiB     uint64 `json:"capacityHeadroomMiB"`
	StatusTimeoutSeconds    int    `json:"statusTimeoutSeconds,omitempty"`
	OperationTimeoutSeconds int    `json:"operationTimeoutSeconds,omitempty"`
}

func (p Profile) Validate() error {
	if p.Version != 1 || !releaseToken(p.ActivatedRelease) || p.GPUIndex < 0 || p.StatusTimeoutSeconds < 0 || p.StatusTimeoutSeconds > 60 || p.OperationTimeoutSeconds < 0 || p.OperationTimeoutSeconds > 1800 {
		return errors.New("invalid deployment profile")
	}
	for _, s := range []string{p.StatePath, p.SystemctlPath, p.NvidiaSMIPath} {
		if !filepath.IsAbs(s) || filepath.Clean(s) != s || s == "/" {
			return errors.New("profile requires absolute clean paths")
		}
	}
	return nil
}

func releaseToken(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (p Profile) SystemdConfig(catalog *control.Catalog) gpuruntime.SystemdConfig {
	return gpuruntime.SystemdConfig{Catalog: catalog, SystemctlPath: p.SystemctlPath, NvidiaSMIPath: p.NvidiaSMIPath, GPUIndex: p.GPUIndex, CapacityHeadroomMiB: p.CapacityHeadroomMiB, HealthTimeout: 10 * time.Second}
}

type Marker struct {
	Version     int    `json:"version"`
	Release     string `json:"release"`
	Maintenance bool   `json:"maintenance"`
}

// Check must run under the controller gate, before opening the database.
// An absent marker permits legacy CLI deployments, never a managed profile.
func Check(statePath, activatedRelease string) error {
	marker, err := Read(statePath)
	if errors.Is(err, os.ErrNotExist) && activatedRelease == "" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deployment activation: %w", err)
	}
	if marker.Maintenance {
		return errors.New("deployment is in maintenance; resume setup")
	}
	if marker.Release != Release || (activatedRelease != "" && marker.Release != activatedRelease) {
		return errors.New("installed release is not activated; run setup (downgrade requires compatible restore)")
	}
	// Managed state is created only by explicit setup. Never let an ordinary
	// opener recreate a missing database or repair an untrusted state path.
	path, err := filepath.Abs(statePath)
	if err != nil {
		return fmt.Errorf("managed state path: %w", err)
	}
	file, err := OpenPrivate(path)
	if err != nil {
		return fmt.Errorf("managed state requires an existing private database: %w", err)
	}
	return file.Close()
}

func Read(statePath string) (Marker, error) {
	var marker Marker
	path, err := filepath.Abs(statePath + Suffix)
	if err != nil {
		return marker, err
	}
	file, err := OpenPrivate(path)
	if err != nil {
		return marker, err
	}
	defer file.Close()
	if err := strictjson.DecodeLimited(file, 4096, &marker); err != nil {
		return marker, err
	}
	if marker.Version != 1 || marker.Release == "" {
		return marker, errors.New("invalid deployment marker")
	}
	return marker, nil
}

// Write atomically and durably replaces a private marker; callers hold both gates.
func Write(statePath string, marker Marker) error {
	if marker.Version != 1 || marker.Release == "" {
		return errors.New("invalid deployment marker")
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return AtomicWrite(statePath+Suffix, data)
}

// AtomicWrite is restricted to setup-owned files in an already trusted directory.
func AtomicWrite(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".setup-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// OpenPrivate walks trusted directories with no-follow descriptors and returns a
// verified regular file. It neither follows a swapped symlink nor blocks on FIFOs.
func OpenPrivate(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("path must be absolute and clean")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			unix.Close(fd)
			return nil, err
		}
		if !trustedPrivateStat(stat, i == len(parts)-1) {
			unix.Close(fd)
			return nil, errors.New("untrusted private file or directory")
		}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func trustedPrivateStat(stat unix.Stat_t, private bool) bool {
	writable := stat.Mode&0022 != 0
	if !private && stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0 {
		writable = false
	}
	return !((stat.Uid != uint32(os.Geteuid()) && (private || stat.Uid != 0)) || writable || (private && (stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0)))
}
