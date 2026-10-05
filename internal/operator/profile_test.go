package operator

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func loadProfileHome(t *testing.T, home string, uid int) (Profile, error) {
	t.Helper()
	fd, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Profile{}, err
	}
	return loadProfileFD(fd, uid)
}

func TestPrivateProfileTrust(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".config", "gpu-workload-supervisor")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "operator.json")
	body := `{"version":1,"statePath":"/var/tmp/state.db","activatedRelease":"v1","systemctlPath":"/usr/bin/systemctl","nvidiaSMIPath":"/usr/bin/nvidia-smi","gpuIndex":0}`
	os.WriteFile(p, []byte(body), 0600)
	if _, e := loadProfileHome(t, root, os.Geteuid()); e != nil {
		t.Fatal(e)
	}
	os.Chmod(p, 0666)
	if _, e := loadProfileHome(t, root, os.Geteuid()); e == nil {
		t.Fatal("writable profile accepted")
	}
	os.Chmod(p, 0600)
	os.Rename(p, p+".real")
	os.Symlink(p+".real", p)
	if _, e := loadProfileHome(t, root, os.Geteuid()); e == nil {
		t.Fatal("symlink accepted")
	}
}
func TestProfileLimits(t *testing.T) {
	p := Profile{Version: 1, StatePath: "/a/state", ActivatedRelease: "v1", SystemctlPath: "/usr/bin/systemctl", NvidiaSMIPath: "/usr/bin/nvidia-smi"}
	if e := ValidateProfile(p); e != nil {
		t.Fatal(e)
	}
	p.StatusTimeoutSeconds = 61
	if ValidateProfile(p) == nil {
		t.Fatal("timeout accepted")
	}
}
func TestProfileRejectsUnsafeAncestorsAndJSON(t *testing.T) {
	root := t.TempDir()
	if _, e := loadProfileHome(t, root, os.Geteuid()); e == nil {
		t.Fatal("missing directory")
	}
	os.MkdirAll(filepath.Join(root, ".config", "gpu-workload-supervisor"), 0700)
	p := filepath.Join(root, ".config", "gpu-workload-supervisor", "operator.json")
	for _, b := range []string{`{}`, `{"version":1,"unknown":1}`, `{"version":1,"version":1}`, `[]`, string(make([]byte, MaxRequestBytes+1))} {
		os.WriteFile(p, []byte(b), 0600)
		if _, e := loadProfileHome(t, root, os.Geteuid()); e == nil {
			t.Fatal("bad JSON accepted")
		}
	}
	os.Chmod(filepath.Join(root, ".config"), 0777)
	if _, e := loadProfileHome(t, root, os.Geteuid()); e == nil {
		t.Fatal("writable parent")
	}
	os.Chmod(filepath.Join(root, ".config"), 0700)
	os.Remove(p)
	os.Mkdir(p, 0700)
	if _, e := loadProfileHome(t, root, os.Geteuid()); e == nil {
		t.Fatal("directory profile")
	}
	if _, e := openDirectoryChain("/tmp", os.Geteuid()); e == nil {
		t.Fatal("writable ancestor")
	}
	if _, e := openDirectoryChain("/nonexistent/operator", os.Geteuid()); e == nil {
		t.Fatal("missing ancestor")
	}
	fd, e := openDirectoryChain("/usr", os.Geteuid())
	if e != nil {
		t.Fatal(e)
	}
	os.NewFile(uintptr(fd), "usr").Close()
	if trustedFD(-1, os.Geteuid(), true) == nil {
		t.Fatal("invalid descriptor")
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	_, _ = LoadProfile() // never adopts the environment path
}
func TestProfileRejectsCaseAliases(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".config", "gpu-workload-supervisor")
	os.MkdirAll(dir, 0700)
	for _, b := range []string{`{"Version":1,"statePath":"/a/db","activatedRelease":"dev","systemctlPath":"/usr/bin/true","nvidiaSMIPath":"/usr/bin/true","gpuIndex":0}`, `{"version":2,"Version":1,"statePath":"/a/db","activatedRelease":"dev","systemctlPath":"/usr/bin/true","nvidiaSMIPath":"/usr/bin/true","gpuIndex":0}`} {
		os.WriteFile(filepath.Join(dir, "operator.json"), []byte(b), 0600)
		if _, e := loadProfileHome(t, root, os.Geteuid()); e == nil {
			t.Fatal("accepted case alias")
		}
	}
}
