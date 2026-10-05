package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Profile contains deployment paths and resource limits, never the live catalog.
type Profile struct {
	Version                 int    `json:"version"`
	StatePath               string `json:"statePath"`
	ActivatedRelease        string `json:"activatedRelease"`
	SystemctlPath           string `json:"systemctlPath"`
	NvidiaSMIPath           string `json:"nvidiaSMIPath"`
	GPUIndex                int    `json:"gpuIndex"`
	CapacityHeadroomMiB     uint64 `json:"capacityHeadroomMiB,omitempty"`
	StatusTimeoutSeconds    int    `json:"statusTimeoutSeconds,omitempty"`
	OperationTimeoutSeconds int    `json:"operationTimeoutSeconds,omitempty"`
}

func ValidateProfile(p Profile) error {
	if p.Version != 1 || !token(p.ActivatedRelease, 128) || p.GPUIndex < 0 || p.StatusTimeoutSeconds < 0 || p.StatusTimeoutSeconds > 60 || p.OperationTimeoutSeconds < 0 || p.OperationTimeoutSeconds > 1800 {
		return errors.New("invalid deployment profile")
	}
	for _, s := range []string{p.StatePath, p.SystemctlPath, p.NvidiaSMIPath} {
		if !filepath.IsAbs(s) || filepath.Clean(s) != s || s == "/" {
			return errors.New("profile requires absolute clean paths")
		}
	}
	return nil
}

// LoadProfile resolves the effective account from the OS database, ignoring HOME/XDG.
func LoadProfile() (Profile, error) {
	u, e := user.LookupId(strconv.Itoa(os.Geteuid()))
	if e != nil {
		return Profile{}, e
	}
	if !filepath.IsAbs(u.HomeDir) || filepath.Clean(u.HomeDir) != u.HomeDir {
		return Profile{}, errors.New("invalid account home")
	}
	fd, e := openDirectoryChain(u.HomeDir, os.Geteuid())
	if e != nil {
		return Profile{}, e
	}
	return loadProfileFD(fd, os.Geteuid())
}
func trustedFD(fd, uid int, dir bool) error {
	var st unix.Stat_t
	if e := unix.Fstat(fd, &st); e != nil {
		return e
	}
	kind := uint32(unix.S_IFREG)
	if dir {
		kind = unix.S_IFDIR
	}
	if st.Mode&unix.S_IFMT != kind || st.Mode&0022 != 0 || int(st.Uid) != uid && (!dir || st.Uid != 0) {
		return errors.New("untrusted profile owner, mode or type")
	}
	return nil
}
func openDirectoryChain(path string, uid int) (int, error) {
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return -1, e
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
		if e = trustedFD(fd, uid, true); e != nil {
			unix.Close(fd)
			return -1, e
		}
	}
	return fd, nil
}
func loadProfileFD(fd, uid int) (Profile, error) {
	var p Profile
	var e error
	if e = trustedFD(fd, uid, true); e != nil {
		unix.Close(fd)
		return p, e
	}
	for _, part := range []string{".config", "gpu-workload-supervisor"} {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return p, e
		}
		fd = next
		if e = trustedFD(fd, uid, true); e != nil {
			unix.Close(fd)
			return p, e
		}
	}
	fileFD, e := unix.Openat(fd, "operator.json", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	unix.Close(fd)
	if e != nil {
		return p, e
	}
	f := os.NewFile(uintptr(fileFD), "operator.json")
	defer f.Close()
	if e = trustedFD(fileFD, uid, false); e != nil {
		return p, e
	}
	b, e := io.ReadAll(io.LimitReader(f, MaxRequestBytes+1))
	if e != nil || len(b) > MaxRequestBytes {
		return p, errors.New("profile read limit")
	}
	if _, ok := fields(b, "version", "statePath", "activatedRelease", "systemctlPath", "nvidiaSMIPath", "gpuIndex", "capacityHeadroomMiB", "statusTimeoutSeconds", "operationTimeoutSeconds"); !ok {
		return p, errors.New("invalid profile fields")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if strictjson.Check(json.NewDecoder(bytes.NewReader(b))) != nil || d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return p, errors.New("invalid profile JSON")
	}
	return p, ValidateProfile(p)
}
