package runtime

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// escapeFixture is a host root where every escape route is closed: the deny
// files list the account, and no spool, group, socket, tool or unit gives
// a route. The account UID differs from the owner of the fixture files.
type escapeFixture struct {
	m    *SystemdManager
	r    *fakeRunner
	root string
	uid  int
	// blocked lists inodes that the walk cannot read.
	blocked map[uint64]error
	// accountOwned lists inodes that ownerOf reports with the account UID.
	accountOwned map[uint64]bool
	// acl lists inodes with an access ACL.
	acl       map[uint64]bool
	gids      []uint32
	gidsErr   error
	socketACL bool
	aclErr    error
}

// exitStatus is a command that ran and exited with a status.
type exitStatus int

func (e exitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

func newEscapeFixture(t *testing.T) *escapeFixture {
	t.Helper()
	r := &fakeRunner{outputs: map[string][]byte{}, errs: map[string]error{}}
	m, err := newSystemdManager(testConfig(), r, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	f := &escapeFixture{m: m, r: r, root: t.TempDir(), uid: os.Getuid() + 4242, blocked: map[uint64]error{}, accountOwned: map[uint64]bool{}, acl: map[uint64]bool{}}
	m.host = limitHost{root: f.root, owner: uint32(os.Getuid()), uid: f.uid, ownerOf: f.ownerOf, hasXattr: f.hasXattr, lstat: func(name string) (unix.Stat_t, error) {
		var stat unix.Stat_t
		err := unix.Lstat(name, &stat)
		return stat, err
	}}
	m.escape = escapeHost{pid: 42, verify: f.verify,
		processGroups: func() ([]uint32, error) { return f.gids, f.gidsErr },
		socketACL:     func(string) (bool, error) { return f.socketACL, f.aclErr }}
	f.write(t, "/etc/passwd", "root:x:0:0::/root:/bin/sh\nalice:x:"+strconv.Itoa(f.uid)+":5000::/home/alice:/bin/sh\n")
	f.write(t, "/etc/group", "root:x:0:\nalice:x:5000:\ndocker:x:5001:bob\nwheel:x:5002:\ndisk:x:5003:\nbad:x:none:\nmalformed\n")
	f.write(t, "/etc/cron.deny", "bob\nalice\n")
	f.write(t, "/etc/at.deny", "alice\n")
	return f
}

func (f *escapeFixture) write(t *testing.T, name, value string) {
	t.Helper()
	writeTree(t, f.root, name, value)
}

func (f *escapeFixture) inode(t *testing.T, name string) uint64 {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Lstat(filepath.Join(f.root, name), &stat); err != nil {
		t.Fatal(err)
	}
	return stat.Ino
}

func (f *escapeFixture) ownerOf(stat unix.Stat_t) uint32 {
	if f.accountOwned[stat.Ino] {
		return uint32(f.uid)
	}
	return stat.Uid
}

func (f *escapeFixture) verify(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	return f.blocked[stat.Ino]
}

func (f *escapeFixture) hasXattr(fd int, attribute string) (bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return false, err
	}
	if f.acl[stat.Ino] && attribute == "system.posix_acl_access" {
		return true, nil
	}
	return hasXattr(fd, attribute)
}

// tool installs an executable under the fixture root and returns the
// command prefix that the runner sees.
func (f *escapeFixture) tool(t *testing.T, name string) string {
	t.Helper()
	f.write(t, name, "#!/bin/sh\n")
	chmod(t, f.root, name, 0o755)
	return filepath.Join(f.root, name)
}

func (f *escapeFixture) check() error {
	return f.m.checkEscapeRoutes(context.Background(), control.WorkloadProfile{})
}

// requireOpen requires a definite open route whose text has each part.
func (f *escapeFixture) requireOpen(t *testing.T, parts ...string) {
	t.Helper()
	err := f.check()
	if !errors.Is(err, ErrEscapeRoute) || !DefiniteLimitFailure(err) || errors.Is(err, ErrLimitReadback) ||
		!strings.HasSuffix(err.Error(), "; close each route, or remove the limits through setup") {
		t.Fatalf("escape routes = %v, want an open route", err)
	}
	for _, part := range parts {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("escape routes = %v, missing %q", err, part)
		}
	}
}

// requireReadback requires a check failure that proves nothing.
func (f *escapeFixture) requireReadback(t *testing.T, part string) {
	t.Helper()
	err := f.check()
	if !errors.Is(err, ErrLimitReadback) || errors.Is(err, ErrEscapeRoute) || DefiniteLimitFailure(err) || !strings.Contains(err.Error(), part) {
		t.Fatalf("escape routes = %v, want a readback failure with %q", err, part)
	}
}

func TestEscapeRoutesClosed(t *testing.T) {
	f := newEscapeFixture(t)
	if err := f.check(); err != nil {
		t.Fatalf("closed routes = %v", err)
	}
	// Spools without jobs of the account, system crontabs with jobs of
	// other users, a readable cron.allow that omits the account, a sudo
	// denial, pkcheck refusals and units of other users keep them closed.
	f.write(t, "/var/spool/cron/bob", "* * * * * true\n")
	f.write(t, "/var/spool/cron/crontabs/bob", "* * * * * true\n")
	f.write(t, "/var/spool/cron/atjobs/a0001", "job\n")
	f.write(t, "/etc/crontab", "SHELL=/bin/sh\nMAILTO = alice\n# * * * * * alice x\n\n* * * * * root run alice\n@reboot bob x\n* * *\n")
	f.write(t, "/etc/cron.d/backup", "0 3 * * * root backup\n")
	f.write(t, "/etc/cron.allow", "bob\nalice2\n")
	f.write(t, "/etc/at.allow", "")
	sudo := f.tool(t, "/usr/bin/sudo") + " -n -l -U alice"
	f.r.outputs[sudo] = []byte("User alice is not allowed to run sudo on host.\n")
	f.r.errs[sudo] = exitStatus(1)
	pkcheck := f.tool(t, "/usr/bin/pkcheck")
	f.r.errs[pkcheck+" --action-id org.freedesktop.systemd1.manage-units --process 42"] = exitStatus(2)
	f.r.errs[pkcheck+" --action-id org.freedesktop.systemd1.manage-unit-files --process 42"] = exitStatus(1)
	f.r.outputs["/usr/bin/true show --property=Id --property=User -- *"] = []byte("Id=user@" + strconv.Itoa(f.uid) + ".service\nUser=" + strconv.Itoa(f.uid) +
		"\n\nId=db.service\nUser=bob\n\nId=sshd.service\nUser=\n\nId=basic.target\n")
	if err := f.check(); err != nil {
		t.Fatalf("closed routes with jobs of other users = %v: %v", err, f.r.calls)
	}
	f.r.outputs[sudo] = []byte("Sorry, user alice may not run sudo on host.\n")
	if err := f.check(); err != nil {
		t.Fatalf("closed routes with the sudo denial of the account = %v", err)
	}
}

func TestCronRouteOpen(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"no allow or deny":           {map[string]string{"/etc/cron.deny": ""}, "cron: /etc/cron.deny does not deny alice"},
		"trailing space":             {map[string]string{"/etc/cron.deny": "alice \n"}, "cron: /etc/cron.deny does not deny alice"},
		"carriage return":            {map[string]string{"/etc/cron.deny": "alice\r\n"}, "cron: /etc/cron.deny does not deny alice"},
		"no newline":                 {map[string]string{"/etc/cron.deny": "bob\nalice"}, "cron: /etc/cron.deny does not deny alice"},
		"allow lists the account":    {map[string]string{"/etc/cron.allow": "alice\n"}, "cron: /etc/cron.allow allows alice"},
		"allow lists it loosely":     {map[string]string{"/etc/cron.allow": " alice\r\n"}, "cron: /etc/cron.allow allows alice"},
		"cronie spool":               {map[string]string{"/var/spool/cron/alice": "* * * * * x\n"}, "cron: /var/spool/cron/alice"},
		"debian spool":               {map[string]string{"/var/spool/cron/crontabs/alice": "* * * * * x\n"}, "cron: /var/spool/cron/crontabs/alice"},
		"system crontab by name":     {map[string]string{"/etc/crontab": "SHELL=/bin/sh\n17 * * * * alice cd / && run\n"}, "cron: /etc/crontab line 2 runs as alice"},
		"cron.d job by number":       {map[string]string{"/etc/cron.d/job": "@reboot UID x\n"}, "cron: /etc/cron.d/job line 1 runs as UID"},
		"cron.d entry not a file":    {map[string]string{"/etc/cron.d/sub/x": ""}, "cron: setup cannot read or trust /etc/cron.d/sub (not a regular file)"},
		"at deny omits the account":  {map[string]string{"/etc/at.deny": "bob\n"}, "at: /etc/at.deny does not deny alice"},
		"at allow lists the account": {map[string]string{"/etc/at.allow": "alice\n"}, "at: /etc/at.allow allows alice"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEscapeFixture(t)
			for file, value := range c.files {
				f.write(t, file, strings.ReplaceAll(value, "UID", strconv.Itoa(f.uid)))
			}
			f.requireOpen(t, strings.ReplaceAll(c.want, "UID", strconv.Itoa(f.uid)))
		})
	}
}

// Without allow and deny files, cron and at are open: the checks need no
// installed binary.
func TestCronAndAtOpenWithoutRuleFiles(t *testing.T) {
	f := newEscapeFixture(t)
	remove(t, f.root, "/etc/cron.deny")
	remove(t, f.root, "/etc/at.deny")
	f.requireOpen(t, "cron: neither /etc/cron.allow nor /etc/cron.deny denies alice", "at: neither /etc/at.allow nor /etc/at.deny denies alice")
}

// Spool entries and queued at jobs count by the owner too.
func TestSpoolJobsOfTheAccountOpenRoutes(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/var/spool/cron/crontabs/renamed", "* * * * * x\n")
	f.write(t, "/var/spool/at/a00012", "job\n")
	f.accountOwned[f.inode(t, "/var/spool/cron/crontabs/renamed")] = true
	f.accountOwned[f.inode(t, "/var/spool/at/a00012")] = true
	f.requireOpen(t, "cron: /var/spool/cron/crontabs/renamed", "at: /var/spool/at/a00012")
}

// Allow and deny files and their parent directories need the trusted
// owner, no group or other write, no ACL and no symlink.
func TestUntrustedRuleFilesOpenRoutes(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(*testing.T, *escapeFixture)
		want  string
	}{
		"owned by the account": {func(t *testing.T, f *escapeFixture) { f.accountOwned[f.inode(t, "/etc/cron.deny")] = true },
			"cron: setup cannot read or trust /etc/cron.deny (untrusted file"},
		"group write":     {func(t *testing.T, f *escapeFixture) { chmod(t, f.root, "/etc/cron.deny", 0o664) }, "/etc/cron.deny (untrusted file"},
		"writable parent": {func(t *testing.T, f *escapeFixture) { chmod(t, f.root, "/etc", 0o777) }, "cron: setup cannot read or trust /etc (untrusted file"},
		"ACL":             {func(t *testing.T, f *escapeFixture) { f.acl[f.inode(t, "/etc/at.deny")] = true }, "at: setup cannot read or trust /etc/at.deny (untrusted file"},
		"untrusted allow": {func(t *testing.T, f *escapeFixture) {
			f.write(t, "/etc/at.allow", "")
			chmod(t, f.root, "/etc/at.allow", 0o666)
		}, "at: setup cannot read or trust /etc/at.allow"},
		"not regular": {func(t *testing.T, f *escapeFixture) {
			remove(t, f.root, "/etc/cron.deny")
			f.write(t, "/etc/cron.deny/x", "")
		}, "/etc/cron.deny (not a regular file)"},
		"too large":        {func(t *testing.T, f *escapeFixture) { f.write(t, "/etc/cron.deny", strings.Repeat("x", policyLimit+1)) }, "/etc/cron.deny (larger than 1 MiB)"},
		"symlink":          {func(t *testing.T, f *escapeFixture) { symlinkRule(t, f, "/etc/cron.deny") }, "/etc/cron.deny (too many levels of symbolic links)"},
		"symlink in spool": {func(t *testing.T, f *escapeFixture) { symlinkRule(t, f, "/var/spool/at") }, "at: setup cannot read or trust /var/spool/at"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEscapeFixture(t)
			c.setup(t, f)
			f.requireOpen(t, c.want)
		})
	}
}

func symlinkRule(t *testing.T, f *escapeFixture, name string) {
	t.Helper()
	target := filepath.Join(f.root, "/etc/at.deny")
	_ = os.RemoveAll(filepath.Join(f.root, name))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.root, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(f.root, name)); err != nil {
		t.Fatal(err)
	}
}

// A policy that setup cannot read leaves its route open. An unreadable
// allow file counts as absent, so the deny file decides.
func TestUnreadablePoliciesOpenRoutes(t *testing.T) {
	for name, c := range map[string]struct {
		files   map[string]string
		blocked string
		want    string
	}{
		"deny":               {nil, "/etc/cron.deny", "cron: setup cannot read or trust /etc/cron.deny (permission denied)"},
		"allow without deny": {map[string]string{"/etc/at.allow": "bob\n"}, "/etc/at.allow", "at: /etc/at.deny does not deny alice"},
		"spool":              {map[string]string{"/var/spool/cron/crontabs/bob": ""}, "/var/spool/cron/crontabs", "cron: setup cannot read or trust /var/spool/cron/crontabs (permission denied)"},
		"at spool":           {map[string]string{"/var/spool/cron/atjobs/a1": ""}, "/var/spool/cron/atjobs", "at: setup cannot read or trust /var/spool/cron/atjobs (permission denied)"},
		"system crontab":     {map[string]string{"/etc/crontab": ""}, "/etc/crontab", "cron: setup cannot read or trust /etc/crontab (permission denied)"},
		"cron.d":             {map[string]string{"/etc/cron.d/job": ""}, "/etc/cron.d", "cron: setup cannot read or trust /etc/cron.d (permission denied)"},
		"passwd":             {nil, "/etc/passwd", "account: setup cannot read or trust /etc/passwd (permission denied)"},
		"group":              {nil, "/etc/group", "groups: setup cannot read or trust /etc/group (permission denied)"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEscapeFixture(t)
			for file, value := range c.files {
				f.write(t, file, value)
			}
			if name == "allow without deny" {
				f.write(t, "/etc/at.deny", "bob\n")
			}
			f.blocked[f.inode(t, c.blocked)] = unix.EACCES
			f.requireOpen(t, c.want)
		})
	}
}

// An unreadable allow file counts as absent: a deny file that lists the
// account still closes the route.
func TestUnreadableAllowFileCountsAsAbsent(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/etc/cron.allow", "alice\n")
	f.blocked[f.inode(t, "/etc/cron.allow")] = unix.EPERM
	if err := f.check(); err != nil {
		t.Fatalf("unreadable cron.allow with a deny = %v", err)
	}
}

// Without the name of the account, setup cannot check the routes by name.
func TestUnknownAccountOpensRoute(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/etc/passwd", "root:x:0:0::/root:/bin/sh\nbad:x:"+strconv.Itoa(f.uid)+":x::/:/bin/sh\n")
	f.r.outputs["/usr/bin/true show --property=Id --property=User -- *"] = []byte("Id=job.service\nUser=" + strconv.Itoa(f.uid) + "\n")
	f.requireOpen(t, "account: /etc/passwd has no entry for uid", "system units: job.service runs as "+strconv.Itoa(f.uid))
}

func TestGroupAndSocketRoutes(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(*testing.T, *escapeFixture)
		want  string
	}{
		"listed member": {func(t *testing.T, f *escapeFixture) { f.write(t, "/etc/group", "docker:x:5001:bob,alice\n") }, "groups: member of docker"},
		"process group": {func(t *testing.T, f *escapeFixture) { f.gids = []uint32{5002} }, "groups: member of wheel"},
		"primary group": {func(t *testing.T, f *escapeFixture) { f.write(t, "/etc/group", "disk:x:5000:\n") }, "groups: member of disk"},
		"group socket":  {func(t *testing.T, f *escapeFixture) { f.socket(t, "/run/docker.sock", 0o660, 5000) }, "sockets: /run/docker.sock is writable by this account"},
		"other socket":  {func(t *testing.T, f *escapeFixture) { f.socket(t, "/run/podman/podman.sock", 0o666, 1) }, "sockets: /run/podman/podman.sock is writable"},
		"owned socket": {func(t *testing.T, f *escapeFixture) {
			f.socket(t, "/var/lib/incus/unix.socket", 0o600, 1)
			f.ownSocket(t, "/var/lib/incus/unix.socket")
		}, "sockets: /var/lib/incus/unix.socket is writable"},
		"socket with ACL": {func(t *testing.T, f *escapeFixture) {
			f.socket(t, "/run/libvirt/libvirt-sock", 0o600, 1)
			f.socketACL = true
		}, "sockets: /run/libvirt/libvirt-sock has an ACL"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEscapeFixture(t)
			c.setup(t, f)
			f.requireOpen(t, c.want)
		})
	}
}

// A socket that the account cannot write, a socket owned by the account
// without owner write and a file that is not a socket are closed.
func TestClosedSockets(t *testing.T) {
	f := newEscapeFixture(t)
	f.socket(t, "/run/docker.sock", 0o660, 1)
	f.socket(t, "/run/containerd/containerd.sock", 0o066, 1)
	f.ownSocket(t, "/run/containerd/containerd.sock")
	f.write(t, "/var/lib/lxd/unix.socket", "")
	chmod(t, f.root, "/var/lib/lxd/unix.socket", 0o666)
	if err := f.check(); err != nil {
		t.Fatalf("closed sockets = %v", err)
	}
}

func (f *escapeFixture) socket(t *testing.T, name string, mode uint32, gid int) {
	t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mknod(path, unix.S_IFSOCK|mode, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if gid == 5000 {
		// The fixture user cannot give a file group 5000, so the account
		// gets the group of the file instead.
		var stat unix.Stat_t
		if err := unix.Stat(path, &stat); err != nil {
			t.Fatal(err)
		}
		f.gids = append(f.gids, stat.Gid)
	}
}

func (f *escapeFixture) ownSocket(t *testing.T, name string) {
	t.Helper()
	f.accountOwned[f.inode(t, name)] = true
}

func TestSudoRoute(t *testing.T) {
	f := newEscapeFixture(t)
	sudo := f.tool(t, "/usr/bin/sudo") + " -n -l -U alice"
	f.r.outputs[sudo] = []byte("Matching Defaults entries for alice on host:\n    env_reset\n\nUser alice may run the following commands on host:\n    (ALL : ALL) ALL\n    (root) NOPASSWD: /usr/bin/\x1b[31mtrue\n")
	f.requireOpen(t, "sudo: rule (ALL : ALL) ALL, sudo: rule (root) NOPASSWD: /usr/bin/[31mtrue")
	f.r.outputs[sudo] = []byte("User alice may run the following commands on host:\n" + strings.Repeat("    (root) /usr/bin/true\n", 7) + "    " + strings.Repeat("x", 130) + "\n")
	f.requireOpen(t, "3 more rules")
	f.r.outputs[sudo] = []byte("User alice may run the following commands on host:\n\n")
	f.requireOpen(t, "sudo: sudo -l lists rules for alice")
	// A password prompt, another language or another denial text is a
	// policy that setup cannot read.
	for _, output := range []string{"sudo: a password is required\n", "User alice is not allowed to run sudo on two hosts.\n", ""} {
		f.r.outputs[sudo] = []byte(output)
		f.r.errs[sudo] = exitStatus(1)
		f.requireOpen(t, "sudo: setup cannot read the sudo policy of alice")
	}
}

// A sudo command that fails or times out proves nothing.
func TestSudoCommandFailureIsReadback(t *testing.T) {
	f := newEscapeFixture(t)
	sudo := f.tool(t, "/usr/bin/sudo") + " -n -l -U alice"
	f.r.errs[sudo] = exitStatus(2)
	f.requireReadback(t, "sudo: /usr/bin/sudo failed")
	f.r.errs[sudo] = context.DeadlineExceeded
	f.requireReadback(t, "sudo: /usr/bin/sudo failed")
	chmod(t, f.root, "/usr/bin/sudo", 0o757)
	f.requireReadback(t, "sudo: /usr/bin/sudo is not a trusted executable")
	f.m.host.lstat = func(string) (unix.Stat_t, error) { return unix.Stat_t{}, unix.EIO }
	f.requireReadback(t, "sudo: /usr/bin/sudo: input/output error")
}

// A canceled context fails the commands, so the check proves nothing.
func TestEscapeRoutesOnCanceledContext(t *testing.T) {
	f := newEscapeFixture(t)
	sudo := f.tool(t, "/usr/bin/sudo") + " -n -l -U alice"
	f.r.errs[sudo] = exitStatus(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := f.m.checkEscapeRoutes(ctx, control.WorkloadProfile{})
	if !errors.Is(err, ErrLimitReadback) || DefiniteLimitFailure(err) {
		t.Fatalf("escape routes on a canceled context = %v", err)
	}
}

func TestPolkitRoute(t *testing.T) {
	f := newEscapeFixture(t)
	pkcheck := f.tool(t, "/bin/pkcheck")
	units := pkcheck + " --action-id org.freedesktop.systemd1.manage-units --process 42"
	files := pkcheck + " --action-id org.freedesktop.systemd1.manage-unit-files --process 42"
	f.r.errs[files] = exitStatus(3)
	f.requireOpen(t, "polkit: a rule lets this account use org.freedesktop.systemd1.manage-units")
	f.r.errs[units] = exitStatus(127)
	f.requireReadback(t, "polkit: /bin/pkcheck failed")
	f.r.errs[units] = exitStatus(1)
	if err := f.check(); err != nil {
		t.Fatalf("polkit that refuses = %v", err)
	}
}

// Without pkcheck, an installed polkit daemon is a policy that setup
// cannot read.
func TestPolkitWithoutPkcheck(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/usr/libexec/polkitd", "")
	f.requireOpen(t, "polkit: /usr/libexec/polkitd exists, but setup cannot read its policy without pkcheck")
	f.m.host.lstat = func(name string) (unix.Stat_t, error) {
		if strings.HasSuffix(name, "polkitd") {
			return unix.Stat_t{}, unix.EIO
		}
		return unix.Stat_t{}, unix.ENOENT
	}
	f.requireReadback(t, "polkit: /usr/lib/polkit-1/polkitd: input/output error")
	f.m.host.lstat = func(name string) (unix.Stat_t, error) {
		if strings.HasSuffix(name, "pkcheck") {
			return unix.Stat_t{}, unix.EIO
		}
		return unix.Stat_t{}, unix.ENOENT
	}
	f.requireReadback(t, "polkit: /usr/bin/pkcheck: input/output error")
}

func TestSystemUnitRoute(t *testing.T) {
	f := newEscapeFixture(t)
	show := "/usr/bin/true show --property=Id --property=User -- *"
	f.r.outputs[show] = []byte("Id=job.service\nUser=alice\n\nId=other.service\nUser=" + strconv.Itoa(f.uid) + "\n")
	f.requireOpen(t, "system units: job.service runs as alice, system units: other.service runs as "+strconv.Itoa(f.uid))
	f.r.outputs[show] = nil
	f.r.errs[show] = errors.New("bus")
	f.requireReadback(t, "system units: systemctl show failed")
}

// A failed check next to an open route does not hide the route: the
// result stays definite.
func TestOpenRouteWinsOverFailedCheck(t *testing.T) {
	f := newEscapeFixture(t)
	f.r.errs["/usr/bin/true show --property=Id --property=User -- *"] = errors.New("bus")
	f.gidsErr = errors.New("groups")
	f.write(t, "/etc/at.deny", "")
	f.requireOpen(t, "at: /etc/at.deny does not deny alice")
	f.write(t, "/etc/at.deny", "alice\n")
	f.requireReadback(t, "groups: groups; system units: systemctl show failed")
}

// A read failure other than a denied read, a symlink or a missing file
// proves nothing.
func TestEscapeRouteReadFailureIsReadback(t *testing.T) {
	f := newEscapeFixture(t)
	f.blocked[f.inode(t, "/etc/cron.deny")] = unix.EIO
	f.requireReadback(t, "cron: /etc/cron.deny (input/output error)")
	delete(f.blocked, f.inode(t, "/etc/cron.deny"))
	f.socket(t, "/run/docker.sock", 0o600, 1)
	f.aclErr = unix.EIO
	f.requireReadback(t, "sockets: /run/docker.sock: input/output error")
}

func TestEscapeHostDefaults(t *testing.T) {
	h := defaultEscapeHost()
	if h.pid != os.Getpid() || h.verify(-1) != nil {
		t.Fatalf("default escape host = %+v", h)
	}
	if gids, err := h.processGroups(); err != nil || len(gids) == 0 || gids[0] != uint32(os.Getgid()) {
		t.Fatalf("process groups = %v, %v", gids, err)
	}
	file := filepath.Join(t.TempDir(), "socket")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if acl, err := h.socketACL(file); acl || err != nil {
		t.Fatalf("ACL of a plain file = %v, %v", acl, err)
	}
	if _, err := h.socketACL(file + ".missing"); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("ACL of a missing file = %v", err)
	}
}
