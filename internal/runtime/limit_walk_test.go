package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// procMemory passes the trust walk, and a read of it at offset 0 fails
// with EIO.
const procMemory = "/proc/self/mem"

// dropInReread is the second read of the drop-ins of a system unit.
func (f *limitFixture) dropInReread(unit string) string {
	return "/usr/bin/true show --property=DropInPaths --property=NeedDaemonReload -- " + unit
}

// The host protection checks use the manager cgroup that they validated:
// they read -.slice one time.
func TestHostProtectionReadsManagerRootOnce(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	if _, _, err := f.m.checkHostProtection(context.Background(), f.p); err != nil {
		t.Fatal(err)
	}
	reads := 0
	for _, call := range f.r.calls {
		if call == rootShowCommand {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("-.slice reads = %d", reads)
	}
}

// A walk does not leave its root through an empty, "." or ".." component.
func TestWalkRefusesNonCanonicalComponents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "cgroup.controllers"), []byte("memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := cgroupFS{root: root, verify: func(int) error { return nil }}
	if _, err := fs.read("/a/b", "cgroup.controllers"); err != nil {
		t.Fatalf("read of a canonical group = %v", err)
	}
	for _, group := range []string{"/a/../a/b", "/a/./b", "/a//b", ""} {
		if _, err := fs.read(group, "cgroup.controllers"); !errors.Is(err, errNonCanonicalPath) {
			t.Errorf("read of %q = %v", group, err)
		}
	}
}

// A listed drop-in that is missing is definite only when a second read
// still lists it and needs no daemon-reload. A package upgrade can remove
// a vendor drop-in between the show call and the walk.
func TestRemovedVendorDropIn(t *testing.T) {
	vendor := "/usr/lib/systemd/system/user-.slice.d/10-defaults.conf"
	for name, tc := range map[string]struct {
		reread   string
		err      error
		definite bool
	}{
		"still listed": {reread: "DropInPaths=" + vendor + " /etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf\nNeedDaemonReload=no\n", definite: true},
		"not listed":   {reread: "DropInPaths=/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf\nNeedDaemonReload=no\n"},
		"needs reload": {reread: "DropInPaths=" + vendor + "\nNeedDaemonReload=yes\n"},
		"read fails":   {err: errors.New("bus")},
	} {
		f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
		remove(t, f.host, vendor)
		f.r.outputs[f.dropInReread("user-1000.slice")] = []byte(tc.reread)
		f.r.errs = map[string]error{f.dropInReread("user-1000.slice"): tc.err}
		err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
		if !errors.Is(err, unix.ENOENT) && tc.definite || DefiniteLimitFailure(err) != tc.definite ||
			errors.Is(err, ErrHostCap) != tc.definite || errors.Is(err, ErrLimitReadback) == tc.definite {
			t.Errorf("%s: missing vendor drop-in = %v", name, err)
		}
	}
}

// Each site that marks a binding read failure keeps the mark. A read of
// /proc/self/mem at offset 0 fails with EIO after the trust walk passes.
func TestBindingReadMarkAtEachSite(t *testing.T) {
	unreadable := "/" + strings.Repeat("r", 300)
	_, linkErr := trustedLinkTarget(unreadable, 1)
	_, readErr := ChatTemplateSHA256(procMemory)
	for name, err := range map[string]error{
		"environment file":   qualifyEnvironmentPath(unreadable, true),
		"search path":        qualifyEnvironmentSearchPath(unreadable),
		"API key read":       checkKeyFile(procMemory),
		"chat template read": readErr,
		"link target":        linkErr,
	} {
		if !errors.Is(err, errBindingRead) || DefiniteBindingFailure(err) {
			t.Errorf("%s with a failed read = %v", name, err)
		}
	}
}

// The root io.cost.qos gets the trust checks of the other host files.
func TestUntrustedIOCostQoSIsHostCap(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	if err := os.Chmod(filepath.Join(f.cgroups, "io.cost.qos"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ioWeightProvider("259:0"); !errors.Is(err, ErrHostCap) {
		t.Fatalf("provider with an untrusted io.cost.qos = %v", err)
	}
}

// The /lib link can also name /usr/lib with an absolute target.
func TestVendorDropInUnderAbsoluteLibLink(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.edit(f.sliceShow(), "DropInPaths=/usr/lib/", "DropInPaths=/lib/")
	if err := os.Symlink("/usr/lib", filepath.Join(f.host, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); err != nil {
		t.Fatalf("launch checks with an absolute /lib link = %v", err)
	}
	if err := os.Remove(filepath.Join(f.host, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/lib", filepath.Join(f.host, "lib")); err != nil {
		t.Fatal(err)
	}
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrHostCap) {
		t.Fatalf("launch checks with a /lib link elsewhere = %v", err)
	}
}

// A trust failure of a parent directory names that directory.
func TestHostFileErrorNamesFailedParent(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	dir := "/etc/systemd/system/user-1000.slice.d"
	if err := os.Chmod(filepath.Join(f.host, dir), 0o775); err != nil {
		t.Fatal(err)
	}
	err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
	if !errors.Is(err, ErrHostCap) || !errors.Is(err, errUntrusted) || !strings.Contains(err.Error(), dir+": untrusted file") {
		t.Fatalf("launch checks with a writable drop-in directory = %v", err)
	}
}
