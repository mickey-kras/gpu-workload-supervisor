package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	sliceVendor   = "/usr/lib/systemd/system/user-.slice.d/10-defaults.conf"
	managerVendor = "/usr/lib/systemd/system/user@.service.d/10-login-barrier.conf"
)

// fixtureDpkg and fixtureRPM are fake package managers. The runner of the
// fixture answers their commands.
var (
	fixtureDpkg = packageTool{name: "dpkg", query: "/fake/dpkg-query", verify: "/fake/dpkg"}
	fixtureRPM  = packageTool{name: "rpm", query: "/fake/rpm", verify: "/fake/rpm"}
)

// The outputs of the fixture package managers, written by hand from the
// dpkg-query(1), dpkg(1) and rpm(8) formats, and from dpkg 1.22 output.
const (
	dpkgOwnerSlice   = "/fake/dpkg-query -S " + sliceVendor
	dpkgOwnerManager = "/fake/dpkg-query -S " + managerVendor
	dpkgMD5Sums      = "/fake/dpkg-query --control-show systemd md5sums"
	dpkgVerify       = "/fake/dpkg --verify-format=rpm --verify systemd"
	rpmOwnerSlice    = "/fake/rpm -qf " + sliceVendor
	rpmVerify        = "/fake/rpm -V --verbose --nodeps --noscripts systemd-255.4-1.fc40.x86_64"
)

// ExitCode gives the exit status, as *exec.ExitError does.
func (e exitStatus) ExitCode() int { return int(e) }

// writePackages gives the fixture an installed systemd package that owns
// both vendor drop-ins with their checksums. Its verify output lists only
// files that are not vendor drop-ins.
func (f *limitFixture) writePackages() {
	if f.r.errs == nil {
		f.r.errs = map[string]error{}
	}
	f.r.outputs[dpkgOwnerSlice] = []byte("systemd: " + sliceVendor + "\n")
	f.r.outputs[dpkgOwnerManager] = []byte("systemd: " + managerVendor + "\n")
	f.r.outputs[dpkgMD5Sums] = []byte("5ab72375af2896e7d7bc8f6ceb06a22e  " + strings.TrimPrefix(sliceVendor, "/") + "\n" +
		"0b0c6fdd2ab7a1b0c3a0e2c3bcd6f1d2  " + strings.TrimPrefix(managerVendor, "/") + "\n")
	f.r.outputs[dpkgVerify] = []byte("missing     /usr/share/doc/systemd/NEWS.gz\n??5?????? c /etc/systemd/logind.conf\n")
}

// useRPM replaces dpkg with rpm, which owns and verifies the slice vendor
// drop-in. The manager vendor drop-in is not owned.
func (f *limitFixture) useRPM(verify string) {
	f.m.host.packageTools = func() ([]packageTool, error) { return []packageTool{fixtureRPM}, nil }
	f.r.outputs[rpmOwnerSlice] = []byte("systemd-255.4-1.fc40.x86_64\n")
	f.r.outputs[rpmVerify] = []byte(verify)
}

func (f *limitFixture) vendorStep(t *testing.T) HostCapStep {
	t.Helper()
	step, err := f.m.HostCapStep(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

// An installed package owns both vendor drop-ins and they match it: the
// launch checks pass and the step shows both as verified.
func TestVendorDropInOwnedAndUnmodified(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); err != nil {
		t.Fatal(err)
	}
	step := f.vendorStep(t)
	want := []VendorDropIn{
		{Unit: "user-1000.slice", Path: sliceVendor, Manager: "dpkg", Packages: []string{"systemd"}, Result: VendorDropInVerified},
		{Unit: "user@1000.service", Path: managerVendor, Manager: "dpkg", Packages: []string{"systemd"}, Result: VendorDropInVerified},
	}
	if step.Status != HostCapPresent || !vendorEqual(step.VendorDropIns, want) {
		t.Fatalf("step = %+v", step)
	}
	for _, call := range []string{dpkgOwnerSlice, dpkgOwnerManager, dpkgMD5Sums, dpkgVerify} {
		if !f.r.called(call) {
			t.Errorf("%s was not called: %v", call, f.r.calls)
		}
	}
}

func vendorEqual(got, want []VendorDropIn) bool {
	return slices.EqualFunc(got, want, func(a, b VendorDropIn) bool {
		return a.Unit == b.Unit && a.Path == b.Path && a.Manager == b.Manager && slices.Equal(a.Packages, b.Packages) && a.Result == b.Result
	})
}

// A vendor drop-in that differs from its package is a definite host cap
// failure, and the step shows it as modified.
func TestVendorDropInModified(t *testing.T) {
	for name, verify := range map[string]string{
		"checksum": "??5??????   " + sliceVendor + "\n",
		"missing":  "missing     " + sliceVendor + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			f.r.outputs[dpkgVerify] = []byte("missing     /usr/share/doc/systemd/NEWS.gz\n" + verify)
			err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
			if !errors.Is(err, ErrHostCap) || !DefiniteLimitFailure(err) || !strings.Contains(err.Error(), "vendor drop-in "+sliceVendor+" differs from package systemd") ||
				!strings.Contains(err.Error(), "drop-in of user-1000.slice") {
				t.Fatalf("launch checks = %v", err)
			}
			step := f.vendorStep(t)
			if step.Status != HostCapDifferent || len(step.VendorDropIns) != 2 || step.VendorDropIns[0].Result != VendorDropInModified ||
				step.VendorDropIns[1].Result != VendorDropInVerified {
				t.Fatalf("step = %+v", step)
			}
		})
	}
}

// A vendor drop-in that no package owns is a definite host cap failure
// whose fix names the host cap drop-in.
func TestVendorDropInUnowned(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.outputs[dpkgOwnerManager] = []byte("dpkg-query: no path found matching pattern " + managerVendor + "\n")
	f.r.errs[dpkgOwnerManager] = exitStatus(1)
	err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
	if !errors.Is(err, ErrHostCap) || !DefiniteLimitFailure(err) ||
		!strings.Contains(err.Error(), "no installed package owns vendor drop-in "+managerVendor+"; remove it, or move its settings into /etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf") {
		t.Fatalf("launch checks = %v", err)
	}
	step := f.vendorStep(t)
	if step.Status != HostCapDifferent || step.VendorDropIns[1].Result != VendorDropInUnowned || step.VendorDropIns[1].Manager != "dpkg" {
		t.Fatalf("step = %+v", step)
	}
}

// Without a supported package manager, the check cannot pass: the launch
// checks fail with a readback failure and the step is unknown.
func TestVendorDropInWithoutPackageManager(t *testing.T) {
	for name, tools := range map[string]func() ([]packageTool, error){
		"none":      func() ([]packageTool, error) { return nil, nil },
		"untrusted": func() ([]packageTool, error) { return nil, errors.New("/usr/bin/rpm cannot be used") },
		"no lookup": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			f.m.host.packageTools = tools
			err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
			if !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) || !strings.Contains(err.Error(), "the package manager cannot check vendor drop-in "+sliceVendor) {
				t.Fatalf("launch checks = %v", err)
			}
			step := f.vendorStep(t)
			if step.Status != HostCapUnknown || len(step.VendorDropIns) != 2 || step.VendorDropIns[0].Result != VendorDropInCannotCheck || step.VendorDropIns[0].Detail == "" {
				t.Fatalf("step = %+v", step)
			}
		})
	}
}

// A package manager command that fails, exits with an error status, prints
// output that the check does not know, or gives no checksum proves nothing.
func TestVendorDropInCommandFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		call   string
		output string
		err    error
		detail string
	}{
		"owner lookup fails":     {call: dpkgOwnerSlice, err: errors.New("fork/exec /fake/dpkg-query: no such file or directory"), detail: "dpkg-query failed"},
		"owner lookup error":     {call: dpkgOwnerSlice, output: "dpkg-query: error: cannot open\n", err: exitStatus(2), detail: "exited with status 2"},
		"owner output unknown":   {call: dpkgOwnerSlice, output: "systemd " + sliceVendor + "\n", detail: "unexpected dpkg-query -S output"},
		"owner output other":     {call: dpkgOwnerSlice, output: "systemd: /usr/lib/other\n", detail: "unexpected dpkg-query -S output"},
		"owner name unknown":     {call: dpkgOwnerSlice, output: "-x: " + sliceVendor + "\n", detail: "unexpected dpkg package name"},
		"owner output empty":     {call: dpkgOwnerSlice, output: "\n", detail: "printed no owner"},
		"diverted":               {call: dpkgOwnerSlice, output: "diversion by other from: " + sliceVendor + "\nsystemd: " + sliceVendor + "\n", detail: "dpkg diverts it"},
		"owner with exit 1":      {call: dpkgOwnerSlice, output: "systemd: " + sliceVendor + "\n", err: exitStatus(1), detail: "exited with status 1"},
		"no checksums":           {call: dpkgMD5Sums, output: "dpkg-query: error: control file 'md5sums' does not exist\n", err: exitStatus(2), detail: "cannot be read"},
		"checksums without file": {call: dpkgMD5Sums, output: "5ab72375af2896e7d7bc8f6ceb06a22e  usr/lib/other\n", detail: "records no checksum of " + sliceVendor},
		"verify error":           {call: dpkgVerify, output: "dpkg: error: unknown verify output format\n", err: exitStatus(2), detail: "dpkg --verify exited with status 2"},
		"verify output unknown":  {call: dpkgVerify, output: "dpkg: warning: files list file for package 'x' missing\n", detail: "unexpected dpkg --verify output"},
		"verify status unknown":  {call: dpkgVerify, output: "??5??   " + sliceVendor + "\n", detail: "unexpected dpkg --verify output"},
		"verify attribute":       {call: dpkgVerify, output: "??5?????? x " + sliceVendor + "\n", detail: "unexpected dpkg --verify output"},
		"verify too many fields": {call: dpkgVerify, output: "??5?????? c d " + sliceVendor + "\n", detail: "unexpected dpkg --verify output"},
		"checksum not compared":  {call: dpkgVerify, output: "?????????   " + sliceVendor + "\n", detail: "the checksum cannot be compared"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			f.r.outputs[tc.call] = []byte(tc.output)
			f.r.errs[tc.call] = tc.err
			err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
			if !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) || errors.Is(err, unix.ENOENT) || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("launch checks = %v", err)
			}
			step := f.vendorStep(t)
			if step.Status != HostCapUnknown || step.VendorDropIns[0].Result != VendorDropInCannotCheck {
				t.Fatalf("step = %+v", step)
			}
		})
	}
}

// blockingRunner holds each package manager command until its context
// ends, as a hung command would.
type blockingRunner struct{ *limitRunner }

func (r blockingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if strings.HasPrefix(name, "/fake/") {
		<-ctx.Done()
		return nil, errors.Join(exitStatus(137), ctx.Err())
	}
	return r.limitRunner.Run(ctx, name, args...)
}

// A package manager command that does not finish ends at its own time
// limit, and the check cannot pass.
func TestVendorDropInCommandTimeout(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.m.runner = blockingRunner{f.r}
	saved := vendorCommandTimeout
	vendorCommandTimeout = 20 * time.Millisecond
	defer func() { vendorCommandTimeout = saved }()
	start := time.Now()
	err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID)
	if !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) || !strings.Contains(err.Error(), "dpkg-query did not finish: context deadline exceeded") {
		t.Fatalf("launch checks = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the check took %v", elapsed)
	}
}

// rpm owns and verifies a vendor drop-in. --verbose lists a passing file,
// so a file that rpm does not list cannot pass.
func TestVendorDropInRPM(t *testing.T) {
	for name, tc := range map[string]struct {
		output, result string
		err            error
	}{
		"verified":        {output: ".........    /usr/lib/other\n........T    " + sliceVendor + "\n", result: VendorDropInVerified},
		"other fails":     {output: "S.5....T.  c /etc/systemd/logind.conf\n.........    " + sliceVendor + "\n", err: exitStatus(1), result: VendorDropInVerified},
		"digest differs":  {output: "S.5....T.    " + sliceVendor + "\n", err: exitStatus(1), result: VendorDropInModified},
		"link differs":    {output: "....L....    " + sliceVendor + "\n", err: exitStatus(1), result: VendorDropInModified},
		"missing":         {output: "missing      " + sliceVendor + "\n", err: exitStatus(1), result: VendorDropInModified},
		"not listed":      {output: ".........    /usr/lib/other\n", result: VendorDropInCannotCheck},
		"ghost":           {output: ".........  g " + sliceVendor + "\n", result: VendorDropInCannotCheck},
		"digest unknown":  {output: "..?......    " + sliceVendor + "\n", result: VendorDropInCannotCheck},
		"modified beats":  {output: "..?......    " + sliceVendor + "\n..5......    " + sliceVendor + "\n", err: exitStatus(1), result: VendorDropInModified},
		"verify fails":    {err: exitStatus(2), result: VendorDropInCannotCheck},
		"metadata change": {output: ".M...UG..    " + sliceVendor + "\n", err: exitStatus(1), result: VendorDropInVerified},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			f.useRPM(tc.output)
			f.r.errs[rpmVerify] = tc.err
			drop := f.m.packageVerdict(context.Background(), sliceVendor)
			if drop.Result != tc.result || drop.Manager != "rpm" || !slices.Equal(drop.Packages, []string{"systemd-255.4-1.fc40.x86_64"}) {
				t.Fatalf("verdict = %+v", drop)
			}
		})
	}
}

// rpm -qf reports a file that no package owns with exit status 1. Other
// failures of the lookup prove nothing.
func TestVendorDropInRPMOwner(t *testing.T) {
	for name, tc := range map[string]struct {
		output, result string
		err            error
	}{
		"unowned":        {output: "file " + sliceVendor + " is not owned by any package\n", err: exitStatus(1), result: VendorDropInUnowned},
		"missing file":   {output: "error: file " + sliceVendor + ": No such file or directory\n", err: exitStatus(1), result: VendorDropInCannotCheck},
		"unknown output": {output: "systemd 255\n", result: VendorDropInCannotCheck},
		"no output":      {result: VendorDropInCannotCheck},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
			f.useRPM("")
			f.r.outputs[rpmOwnerSlice] = []byte(tc.output)
			f.r.errs[rpmOwnerSlice] = tc.err
			if drop := f.m.packageVerdict(context.Background(), sliceVendor); drop.Result != tc.result || drop.Manager != "rpm" {
				t.Fatalf("verdict = %+v", drop)
			}
		})
	}
}

// When dpkg owns no file, rpm is asked next.
func TestVendorDropInAsksTheNextPackageManager(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.useRPM(".........    " + sliceVendor + "\n")
	f.m.host.packageTools = func() ([]packageTool, error) { return []packageTool{fixtureDpkg, fixtureRPM}, nil }
	f.r.outputs[dpkgOwnerSlice] = []byte("dpkg-query: no path found matching pattern " + sliceVendor + "\n")
	f.r.errs[dpkgOwnerSlice] = exitStatus(1)
	if drop := f.m.packageVerdict(context.Background(), sliceVendor); drop.Result != VendorDropInVerified || drop.Manager != "rpm" {
		t.Fatalf("verdict = %+v", drop)
	}
	f.r.outputs[rpmOwnerSlice] = []byte("file " + sliceVendor + " is not owned by any package\n")
	f.r.errs[rpmOwnerSlice] = exitStatus(1)
	if drop := f.m.packageVerdict(context.Background(), sliceVendor); drop.Result != VendorDropInUnowned || drop.Manager != "rpm" {
		t.Fatalf("verdict = %+v", drop)
	}
}

// A Debian package from before the merged /usr move records the file
// under /lib. With a trusted /lib link, the lookup tries that name next.
func TestVendorDropInRecordedUnderLib(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	libName := "/lib/systemd/system/user-.slice.d/10-defaults.conf"
	f.r.outputs[dpkgOwnerSlice] = []byte("dpkg-query: no path found matching pattern " + sliceVendor + "\n")
	f.r.errs[dpkgOwnerSlice] = exitStatus(1)
	f.r.outputs["/fake/dpkg-query -S "+libName] = []byte("systemd: " + libName + "\n")
	f.r.outputs[dpkgMD5Sums] = []byte("5ab72375af2896e7d7bc8f6ceb06a22e  lib/systemd/system/user-.slice.d/10-defaults.conf\n")
	if drop := f.m.packageVerdict(context.Background(), sliceVendor); drop.Result != VendorDropInUnowned {
		t.Fatalf("verdict without a /lib link = %+v", drop)
	}
	if err := os.Symlink("usr/lib", filepath.Join(f.host, "lib")); err != nil {
		t.Fatal(err)
	}
	if drop := f.m.packageVerdict(context.Background(), sliceVendor); drop.Result != VendorDropInVerified {
		t.Fatalf("verdict = %+v", drop)
	}
	f.r.outputs[dpkgVerify] = []byte("??5??????   " + libName + "\n")
	if drop := f.m.packageVerdict(context.Background(), sliceVendor); drop.Result != VendorDropInModified {
		t.Fatalf("verdict of a changed file = %+v", drop)
	}
}

// Each owner of a shared file must pass.
func TestVendorDropInSharedFile(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	f.r.outputs[dpkgOwnerSlice] = []byte("systemd, libsystemd0:amd64: " + sliceVendor + "\n")
	f.r.outputs["/fake/dpkg-query --control-show libsystemd0:amd64 md5sums"] = f.r.outputs[dpkgMD5Sums]
	secondVerify := "/fake/dpkg --verify-format=rpm --verify libsystemd0:amd64"
	f.r.outputs[secondVerify] = []byte("??5??????   " + sliceVendor + "\n")
	drop := f.m.packageVerdict(context.Background(), sliceVendor)
	if drop.Result != VendorDropInModified || !slices.Equal(drop.Packages, []string{"systemd", "libsystemd0:amd64"}) {
		t.Fatalf("verdict with a failed second owner = %+v", drop)
	}
	f.r.outputs[secondVerify] = []byte{}
	drop = f.m.packageVerdict(context.Background(), sliceVendor)
	if drop.Result != VendorDropInVerified || !slices.Equal(drop.Packages, []string{"systemd", "libsystemd0:amd64"}) {
		t.Fatalf("verdict = %+v", drop)
	}
}

// A name with a pattern character is not looked up: dpkg-query -S takes a
// pattern.
func TestVendorDropInPatternName(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	calls := len(f.r.calls)
	if drop := f.m.packageVerdict(context.Background(), "/usr/lib/systemd/system/user-.slice.d/1*.conf"); drop.Result != VendorDropInCannotCheck || len(f.r.calls) != calls {
		t.Fatalf("verdict = %+v, calls %v", drop, f.r.calls[calls:])
	}
}

// A verified file keeps its result while it is unchanged, so the health
// checks do not run the package manager each time. A change to the file
// runs the check again. A failed result is not kept.
func TestVendorDropInCache(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	countOwner := func() int {
		f.r.mu.Lock()
		defer f.r.mu.Unlock()
		count := 0
		for _, call := range f.r.calls {
			if call == dpkgOwnerSlice {
				count++
			}
		}
		return count
	}
	for range 2 {
		if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); err != nil {
			t.Fatal(err)
		}
	}
	if count := countOwner(); count != 1 {
		t.Fatalf("owner lookups = %d", count)
	}
	// A new file with the same text has another inode and ctime.
	remove(t, f.host, sliceVendor)
	f.file(t, sliceVendor, "[Slice]\nTasksMax=33%\n")
	f.r.outputs[dpkgVerify] = []byte("??5??????   " + sliceVendor + "\n")
	for range 2 {
		if err := f.m.VerifyLaunchLimits(context.Background(), f.p.ID); !errors.Is(err, ErrHostCap) {
			t.Fatalf("launch checks after a change = %v", err)
		}
	}
	if count := countOwner(); count != 3 {
		t.Fatalf("owner lookups after a change = %d", count)
	}
}

// The step checks a vendor drop-in that fails the trust walk as cannot
// check, and it skips a listed name that is not a vendor file: the setup
// checks refuse it.
func TestVendorDropInStepEdges(t *testing.T) {
	f := newLimitFixture(t, fixtureLimits(), fixtureLoaded)
	chmod(t, f.host, sliceVendor, 0o666)
	f.edit(f.dropInReread("user@1000.service"), "DropInPaths=", "DropInPaths=/etc/systemd/system.control/user@1000.service.d/50-x.conf ")
	step := f.vendorStep(t)
	if step.Status != HostCapDifferent || len(step.VendorDropIns) != 2 || step.VendorDropIns[0].Result != VendorDropInCannotCheck ||
		!strings.Contains(step.VendorDropIns[0].Detail, "untrusted file") || step.VendorDropIns[1].Path != managerVendor {
		t.Fatalf("step = %+v", step)
	}
	f.r.errs[f.dropInReread("user-1000.slice")] = errors.New("bus error")
	step = f.vendorStep(t)
	if !slices.ContainsFunc(step.Problems, func(p string) bool {
		return strings.HasPrefix(p, "the vendor drop-ins of user-1000.slice cannot be listed")
	}) {
		t.Fatalf("problems = %q", step.Problems)
	}
}

// The package managers of the host are the candidates whose executables
// exist and pass the executable trust check. An executable that fails the
// check is an error, not an absent package manager.
func TestFindPackageTools(t *testing.T) {
	saved := packageToolCandidates
	defer func() { packageToolCandidates = saved }()
	dir := t.TempDir()
	packageToolCandidates = []packageTool{
		{name: "dpkg", query: "/usr/bin/true", verify: "/usr/bin/true"},
		{name: "rpm", query: filepath.Join(dir, "rpm"), verify: filepath.Join(dir, "rpm")},
	}
	tools, err := findPackageTools()
	if err != nil || len(tools) != 1 || tools[0].name != "dpkg" {
		t.Fatalf("tools = %+v, %v", tools, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rpm"), []byte("#!/bin/sh\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "rpm"), 0o777); err != nil {
		t.Fatal(err)
	}
	if tools, err := findPackageTools(); err == nil || !strings.Contains(err.Error(), "cannot be used") {
		t.Fatalf("tools with an untrusted rpm = %+v, %v", tools, err)
	}
}
