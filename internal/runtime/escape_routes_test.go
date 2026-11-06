package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// escapeFixture is a host root where the scan finds no route: the deny
// files list the account, and no spool, group, socket, polkit rule or unit
// gives a route. The account UID differs from the owner of the fixture
// files.
type escapeFixture struct {
	s    escapeScanner
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

const unitShow = "/usr/bin/systemctl show --all --property=Id --property=User -- *"

func newEscapeFixture(t *testing.T) *escapeFixture {
	t.Helper()
	r := &fakeRunner{outputs: map[string][]byte{}, errs: map[string]error{}}
	f := &escapeFixture{r: r, root: t.TempDir(), uid: os.Getuid() + 4242, blocked: map[uint64]error{}, accountOwned: map[uint64]bool{}, acl: map[uint64]bool{}}
	host := limitHost{root: f.root, owner: uint32(os.Getuid()), uid: f.uid, ownerOf: f.ownerOf, hasXattr: f.hasXattr}
	probe := escapeProbe{verify: f.verify,
		processGroups: func() ([]uint32, error) { return f.gids, f.gidsErr },
		socketACL:     func(string) (bool, error) { return f.socketACL, f.aclErr }}
	f.s = escapeScanner{host: host, probe: probe, run: r.Run, systemctl: "/usr/bin/systemctl"}
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

// report gives each finding as "route: item".
func (f *escapeFixture) report() []string {
	var lines []string
	for _, route := range f.s.scan(context.Background()) {
		lines = append(lines, route.Route+": "+route.Item)
	}
	return lines
}

// requireReport requires a finding that contains each part.
func (f *escapeFixture) requireReport(t *testing.T, parts ...string) {
	t.Helper()
	lines := f.report()
	text := strings.Join(lines, "\n")
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Fatalf("escape routes = %q, missing %q", lines, part)
		}
	}
}

func (f *escapeFixture) requireNone(t *testing.T) {
	t.Helper()
	if lines := f.report(); len(lines) != 0 {
		t.Fatalf("escape routes = %q, want none", lines)
	}
}

func TestEscapeRoutesClosed(t *testing.T) {
	f := newEscapeFixture(t)
	f.requireNone(t)
	// Spools without jobs of the account, system crontabs with jobs of
	// other users, a readable cron.allow that omits the account, polkit
	// rules of other actions and units of other users report nothing.
	f.write(t, "/var/spool/cron/bob", "* * * * * true\n")
	f.write(t, "/var/spool/cron/crontabs/bob", "* * * * * true\n")
	f.write(t, "/var/spool/cron/atjobs/a0001", "job\n")
	f.write(t, "/etc/crontab", "SHELL=/bin/sh\nMAILTO = alice\n# * * * * * alice x\n\n* * * * * root run alice\n@reboot bob x\n* * *\n")
	f.write(t, "/etc/cron.d/backup", "0 3 * * * root backup\n")
	f.write(t, "/etc/cron.allow", "bob\nalice2\n")
	f.write(t, "/etc/at.allow", "")
	f.write(t, "/etc/polkit-1/rules.d/50-network.rules", "action.id == \"org.freedesktop.NetworkManager.settings.modify.system\"\n")
	f.write(t, "/usr/share/polkit-1/rules.d/README", "org.freedesktop.systemd1.manage-units\n")
	f.r.outputs[unitShow] = []byte("Id=user@" + strconv.Itoa(f.uid) + ".service\nUser=" + strconv.Itoa(f.uid) +
		"\n\nId=db.service\nUser=bob\n\nId=sshd.service\nUser=\n\nId=basic.target\n")
	f.requireNone(t)
}

func TestCronAndAtRoutes(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"empty deny":                 {map[string]string{"/etc/cron.deny": ""}, "cron: /etc/cron.deny does not deny alice"},
		"trailing space":             {map[string]string{"/etc/cron.deny": "alice \n"}, "cron: /etc/cron.deny does not deny alice"},
		"carriage return":            {map[string]string{"/etc/cron.deny": "alice\r\n"}, "cron: /etc/cron.deny does not deny alice"},
		"no newline":                 {map[string]string{"/etc/cron.deny": "bob\nalice"}, "cron: /etc/cron.deny does not deny alice"},
		"allow lists the account":    {map[string]string{"/etc/cron.allow": "alice\n"}, "cron: /etc/cron.allow allows alice"},
		"allow lists it loosely":     {map[string]string{"/etc/cron.allow": " alice\r\n"}, "cron: /etc/cron.allow allows alice"},
		"cronie spool":               {map[string]string{"/var/spool/cron/alice": "* * * * * x\n"}, "cron: /var/spool/cron/alice"},
		"debian spool":               {map[string]string{"/var/spool/cron/crontabs/alice": "* * * * * x\n"}, "cron: /var/spool/cron/crontabs/alice"},
		"system crontab by name":     {map[string]string{"/etc/crontab": "SHELL=/bin/sh\n17 * * * * alice cd / && run\n"}, "cron: /etc/crontab line 2 runs as alice"},
		"cron.d job by number":       {map[string]string{"/etc/cron.d/job": "@reboot UID x\n"}, "cron: /etc/cron.d/job line 1 runs as UID"},
		"at deny omits the account":  {map[string]string{"/etc/at.deny": "bob\n"}, "at: /etc/at.deny does not deny alice"},
		"at allow lists the account": {map[string]string{"/etc/at.allow": "alice\n"}, "at: /etc/at.allow allows alice"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEscapeFixture(t)
			for file, value := range c.files {
				f.write(t, file, strings.ReplaceAll(value, "UID", strconv.Itoa(f.uid)))
			}
			f.requireReport(t, strings.ReplaceAll(c.want, "UID", strconv.Itoa(f.uid)))
		})
	}
}

// Without allow and deny files, Debian cron lets each user in, so cron is
// reported. at then lets only root in, so its rule reports nothing. The
// scan needs no installed binary.
func TestCronAndAtWithoutRuleFiles(t *testing.T) {
	f := newEscapeFixture(t)
	remove(t, f.root, "/etc/cron.deny")
	remove(t, f.root, "/etc/at.deny")
	lines := f.report()
	if len(lines) != 1 || lines[0] != "cron: neither /etc/cron.allow nor /etc/cron.deny denies alice" {
		t.Fatalf("escape routes without rule files = %q", lines)
	}
}

// Spool entries and queued at jobs count by the owner too.
func TestSpoolJobsOfTheAccount(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/var/spool/cron/crontabs/renamed", "* * * * * x\n")
	f.write(t, "/var/spool/at/a00012", "job\n")
	f.accountOwned[f.inode(t, "/var/spool/cron/crontabs/renamed")] = true
	f.accountOwned[f.inode(t, "/var/spool/at/a00012")] = true
	f.requireReport(t, "cron: /var/spool/cron/crontabs/renamed", "at: /var/spool/at/a00012")
}

// Allow and deny files and their parent directories need the trusted
// owner, no group or other write, no ACL and no symlink. Otherwise the
// scan cannot check them.
func TestUntrustedRuleFiles(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(*testing.T, *escapeFixture)
		want  string
	}{
		"owned by the account": {func(t *testing.T, f *escapeFixture) { f.accountOwned[f.inode(t, "/etc/cron.deny")] = true },
			"cron: cannot check /etc/cron.deny (untrusted file"},
		"group write":     {func(t *testing.T, f *escapeFixture) { chmod(t, f.root, "/etc/cron.deny", 0o664) }, "/etc/cron.deny (untrusted file"},
		"writable parent": {func(t *testing.T, f *escapeFixture) { chmod(t, f.root, "/etc", 0o777) }, "cron: cannot check /etc (untrusted file"},
		"ACL":             {func(t *testing.T, f *escapeFixture) { f.acl[f.inode(t, "/etc/at.deny")] = true }, "at: cannot check /etc/at.deny (untrusted file"},
		"untrusted allow": {func(t *testing.T, f *escapeFixture) {
			f.write(t, "/etc/at.allow", "")
			chmod(t, f.root, "/etc/at.allow", 0o666)
		}, "at: cannot check /etc/at.allow"},
		"not regular": {func(t *testing.T, f *escapeFixture) {
			remove(t, f.root, "/etc/cron.deny")
			f.write(t, "/etc/cron.deny/x", "")
		}, "/etc/cron.deny (not a regular file)"},
		"too large":        {func(t *testing.T, f *escapeFixture) { f.write(t, "/etc/cron.deny", strings.Repeat("x", policyLimit+1)) }, "/etc/cron.deny (larger than 1 MiB)"},
		"symlink":          {func(t *testing.T, f *escapeFixture) { symlinkRule(t, f, "/etc/cron.deny") }, "/etc/cron.deny (too many levels of symbolic links)"},
		"symlink in spool": {func(t *testing.T, f *escapeFixture) { symlinkRule(t, f, "/var/spool/at") }, "at: cannot check /var/spool/at"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEscapeFixture(t)
			c.setup(t, f)
			f.requireReport(t, c.want)
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

// A source that the account cannot read gives "cannot check". For an
// unreadable allow file, the scan does not read the deny file.
func TestUnreadableSources(t *testing.T) {
	for name, c := range map[string]struct {
		files   map[string]string
		blocked string
		want    []string
	}{
		"deny":           {nil, "/etc/cron.deny", []string{"cron: cannot check /etc/cron.deny (permission denied)"}},
		"allow":          {map[string]string{"/etc/at.allow": "bob\n"}, "/etc/at.allow", []string{"at: cannot check /etc/at.allow (permission denied)"}},
		"spool":          {map[string]string{"/var/spool/cron/crontabs/bob": ""}, "/var/spool/cron/crontabs", []string{"cron: cannot check /var/spool/cron/crontabs (permission denied)"}},
		"at spool":       {map[string]string{"/var/spool/cron/atjobs/a1": ""}, "/var/spool/cron/atjobs", []string{"at: cannot check /var/spool/cron/atjobs (permission denied)"}},
		"system crontab": {map[string]string{"/etc/crontab": ""}, "/etc/crontab", []string{"cron: cannot check /etc/crontab (permission denied)"}},
		"cron.d":         {map[string]string{"/etc/cron.d/job": ""}, "/etc/cron.d", []string{"cron: cannot check /etc/cron.d (permission denied)"}},
		"passwd":         {nil, "/etc/passwd", []string{"account: cannot check /etc/passwd (permission denied)"}},
		"polkit rules":   {map[string]string{"/etc/polkit-1/rules.d/x.rules": ""}, "/etc/polkit-1/rules.d", []string{"polkit: cannot check /etc/polkit-1/rules.d (permission denied)"}},
		// The routes by name still run without /etc/group.
		"group": {map[string]string{"/etc/cron.deny": ""}, "/etc/group", []string{"groups: cannot check /etc/group (permission denied)", "cron: /etc/cron.deny does not deny alice"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEscapeFixture(t)
			for file, value := range c.files {
				f.write(t, file, value)
			}
			f.blocked[f.inode(t, c.blocked)] = unix.EACCES
			f.requireReport(t, c.want...)
		})
	}
}

// An allow file that the account cannot read can list the account, so a
// deny file that lists it does not decide.
func TestUnreadableAllowFileCannotBeChecked(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/etc/cron.allow", "alice\n")
	f.blocked[f.inode(t, "/etc/cron.allow")] = unix.EPERM
	if lines := f.report(); len(lines) != 1 || lines[0] != "cron: cannot check /etc/cron.allow (operation not permitted)" {
		t.Fatalf("escape routes with an unreadable cron.allow = %q", lines)
	}
}

// Another read failure also gives "cannot check": the scan is advisory.
func TestReadFailureCannotCheck(t *testing.T) {
	f := newEscapeFixture(t)
	f.blocked[f.inode(t, "/etc/cron.deny")] = unix.EIO
	f.requireReport(t, "cron: cannot check /etc/cron.deny (input/output error)")
}

// Without the name of the account, the routes by name cannot be checked.
// The routes by number still are, and /etc/group still names the process
// groups.
func TestUnknownAccount(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/etc/passwd", "root:x:0:0::/root:/bin/sh\nbad:x:"+strconv.Itoa(f.uid)+":x::/:/bin/sh\n")
	f.write(t, "/etc/group", "docker:x:5001:alice\nwheel:x:5002:\n")
	f.write(t, "/etc/at.deny", "bob\n")
	f.gids = []uint32{5002}
	f.r.outputs[unitShow] = []byte("Id=job.service\nUser=" + strconv.Itoa(f.uid) + "\n\nId=other.service\nUser=\n")
	f.requireReport(t, "account: cannot check cron, at and the member lists of /etc/group: /etc/passwd has no entry for uid",
		"system-units: job.service runs as "+strconv.Itoa(f.uid), "groups: member of wheel")
	if strings.Contains(strings.Join(f.report(), "\n"), "docker") {
		t.Fatalf("escape routes without a name matched a member list: %q", f.report())
	}
	if report := "\n" + strings.Join(f.report(), "\n"); strings.Contains(report, "\ncron:") || strings.Contains(report, "\nat:") {
		t.Fatalf("escape routes without a name checked cron or at: %q", f.report())
	}
}

// When /etc/passwd cannot be read, the report also says which routes the
// scan cannot check.
func TestUnreadablePasswd(t *testing.T) {
	f := newEscapeFixture(t)
	f.blocked[f.inode(t, "/etc/passwd")] = unix.EACCES
	f.requireReport(t, "account: cannot check /etc/passwd (permission denied)",
		"account: cannot check cron, at and the member lists of /etc/group: /etc/passwd cannot be read")
}

// cron does not run directories, hidden files and backup files in
// /etc/cron.d, so the scan skips them. A symlink there cannot be checked.
func TestCronDirectoryEntries(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/etc/cron.d/sub/x", "* * * * * alice x\n")
	f.write(t, "/etc/cron.d/.hidden", "* * * * * alice x\n")
	f.write(t, "/etc/cron.d/job~", "* * * * * alice x\n")
	f.requireNone(t)
	if err := os.Symlink("/etc/crontab", filepath.Join(f.root, "/etc/cron.d/link")); err != nil {
		t.Fatal(err)
	}
	f.requireReport(t, "cron: cannot check /etc/cron.d/link (too many levels of symbolic links)")
}

// One unreadable parent of two cron spools gives one cron item. The at
// queue below it gives its own at item.
func TestEscapeRouteItemsAreUnique(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/var/spool/cron/crontabs/bob", "")
	f.blocked[f.inode(t, "/var/spool/cron")] = unix.EACCES
	lines := f.report()
	if len(lines) != 2 || lines[0] != "cron: cannot check /var/spool/cron (permission denied)" || lines[1] != "at: cannot check /var/spool/cron (permission denied)" {
		t.Fatalf("escape routes with an unreadable spool parent = %q", lines)
	}
}

func TestGroupAndSocketRoutes(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(*testing.T, *escapeFixture)
		want  string
	}{
		"listed member": {func(t *testing.T, f *escapeFixture) { f.write(t, "/etc/group", "docker:x:5001:bob,alice\n") }, "groups: member of docker"},
		"process group": {func(t *testing.T, f *escapeFixture) { f.gids = []uint32{5002} }, "groups: member of wheel"},
		"primary group": {func(t *testing.T, f *escapeFixture) { f.write(t, "/etc/group", "disk:x:5000:\n") }, "groups: member of disk"},
		"group socket":  {func(t *testing.T, f *escapeFixture) { f.socket(t, "/run/docker.sock", 0o660, true) }, "sockets: /run/docker.sock is writable by this account"},
		"other socket":  {func(t *testing.T, f *escapeFixture) { f.socket(t, "/run/podman/podman.sock", 0o666, false) }, "sockets: /run/podman/podman.sock is writable"},
		"owned socket": {func(t *testing.T, f *escapeFixture) {
			f.socket(t, "/var/lib/incus/unix.socket", 0o600, false)
			f.accountOwned[f.inode(t, "/var/lib/incus/unix.socket")] = true
		}, "sockets: /var/lib/incus/unix.socket is writable"},
		"socket with ACL": {func(t *testing.T, f *escapeFixture) {
			f.socket(t, "/run/libvirt/libvirt-sock", 0o600, false)
			f.socketACL = true
		}, "sockets: /run/libvirt/libvirt-sock has an ACL"},
		"socket ACL failure": {func(t *testing.T, f *escapeFixture) {
			f.socket(t, "/run/docker.sock", 0o600, false)
			f.aclErr = unix.EIO
		}, "sockets: cannot check /run/docker.sock (input/output error)"},
		"symlinked socket": {func(t *testing.T, f *escapeFixture) {
			f.socket(t, "/run/real.sock", 0o666, false)
			if err := os.Symlink("real.sock", filepath.Join(f.root, "/run/docker.sock")); err != nil {
				t.Fatal(err)
			}
		}, "sockets: /run/docker.sock is writable by this account"},
		"socket symlink loop": {func(t *testing.T, f *escapeFixture) {
			if err := os.MkdirAll(filepath.Join(f.root, "/run"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("docker.sock", filepath.Join(f.root, "/run/docker.sock")); err != nil {
				t.Fatal(err)
			}
		}, "sockets: cannot check /run/docker.sock (too many levels of symbolic links)"},
		"process groups failure": {func(t *testing.T, f *escapeFixture) { f.gidsErr = errors.New("no groups") }, "groups: cannot check the groups of this process (no groups)"},
		// The process groups still count when /etc/group cannot be read.
		"socket without /etc/group": {func(t *testing.T, f *escapeFixture) {
			f.socket(t, "/run/docker.sock", 0o660, true)
			f.blocked[f.inode(t, "/etc/group")] = unix.EACCES
		}, "sockets: /run/docker.sock is writable by this account"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEscapeFixture(t)
			c.setup(t, f)
			f.requireReport(t, c.want)
		})
	}
}

// A socket that the account cannot write, a socket owned by the account
// without owner write, a group socket without group write, a file that is
// not a socket and a file where a socket directory must be are closed.
func TestClosedSockets(t *testing.T) {
	f := newEscapeFixture(t)
	f.socket(t, "/run/docker.sock", 0o600, false)
	f.socket(t, "/run/containerd/containerd.sock", 0o066, false)
	f.accountOwned[f.inode(t, "/run/containerd/containerd.sock")] = true
	f.socket(t, "/run/podman/podman.sock", 0o606, true)
	f.write(t, "/var/lib/lxd/unix.socket", "")
	chmod(t, f.root, "/var/lib/lxd/unix.socket", 0o666)
	// A file where a socket directory must be hides the socket.
	f.write(t, "/run/libvirt", "")
	f.requireNone(t)
}

// socket makes a socket file. With member, the account gets the group of
// the file, because the fixture user cannot give a file another group.
func (f *escapeFixture) socket(t *testing.T, name string, mode uint32, member bool) {
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
	if member {
		var stat unix.Stat_t
		if err := unix.Stat(path, &stat); err != nil {
			t.Fatal(err)
		}
		f.gids = append(f.gids, stat.Gid)
	}
}

func TestPolkitRules(t *testing.T) {
	f := newEscapeFixture(t)
	f.write(t, "/etc/polkit-1/rules.d/10-units.rules", "if (action.id == \"org.freedesktop.systemd1.manage-unit-files\") { return polkit.Result.YES; }\n")
	f.write(t, "/usr/share/polkit-1/rules.d/20-shell.rules", "org.freedesktop.machine1.host-shell org.freedesktop.policykit.exec\n")
	f.write(t, "/run/polkit-1/rules.d/30-run.rules", "org.freedesktop.systemd1.manage-units\n")
	f.write(t, "/usr/local/share/polkit-1/rules.d/40-local.rules", "org.freedesktop.policykit.exec\n")
	f.requireReport(t, "polkit: /etc/polkit-1/rules.d/10-units.rules mentions org.freedesktop.systemd1.manage-unit",
		"polkit: /run/polkit-1/rules.d/30-run.rules mentions org.freedesktop.systemd1.manage-unit",
		"polkit: /usr/local/share/polkit-1/rules.d/40-local.rules mentions org.freedesktop.policykit.exec",
		"polkit: /usr/share/polkit-1/rules.d/20-shell.rules mentions org.freedesktop.machine1",
		"polkit: /usr/share/polkit-1/rules.d/20-shell.rules mentions org.freedesktop.policykit.exec")
}

// The scan asks systemctl for all loaded units, so inactive units count
// too, such as a timer-activated service that does not run now.
func TestSystemUnitRoute(t *testing.T) {
	f := newEscapeFixture(t)
	f.r.outputs[unitShow] = []byte("Id=job.service\nUser=alice\n\nId=nightly.service\nUser=" + strconv.Itoa(f.uid) + "\n")
	f.requireReport(t, "system-units: job.service runs as alice", "system-units: nightly.service runs as "+strconv.Itoa(f.uid))
	if f.r.calls[len(f.r.calls)-1] != unitShow {
		t.Fatalf("system unit command = %v", f.r.calls)
	}
	f.r.outputs[unitShow] = nil
	f.r.errs[unitShow] = errors.New("bus\nsecond line")
	f.requireReport(t, "system-units: cannot check the loaded system units (systemctl show: bus)")
	f.r.errs[unitShow] = errors.New(strings.Repeat("x", 200))
	f.requireReport(t, "(systemctl show: "+strings.Repeat("x", 160)+"...)")
	// The first line of the systemctl output tells why it failed.
	f.r.outputs[unitShow] = []byte("\nSystem has not been booted with systemd.\nmore\n")
	f.r.errs[unitShow] = exitStatus(1)
	f.requireReport(t, "system-units: cannot check the loaded system units (systemctl show: exit status 1: System has not been booted with systemd.)")
}

// exitStatus is a command that ran and exited with a status.
type exitStatus int

func (e exitStatus) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// firstLine cuts a long line at a character boundary.
func TestFirstLineKeepsCharacters(t *testing.T) {
	line := strings.Repeat("x", 159) + "é" + "tail"
	if got := firstLine(line); got != strings.Repeat("x", 159)+"..." {
		t.Fatalf("firstLine = %q", got)
	}
}

// The report has at most maxEscapeRoutes entries. "cannot check" items
// come first, and the last entry gives the number and the routes of the
// findings left out.
func TestEscapeRouteReportIsBounded(t *testing.T) {
	f := newEscapeFixture(t)
	for i := range 40 {
		f.write(t, "/etc/cron.d/job"+strconv.Itoa(i), "* * * * * alice x\n")
	}
	f.write(t, "/etc/at.deny", "")
	f.r.errs[unitShow] = errors.New("bus")
	routes := f.s.scan(context.Background())
	if len(routes) != maxEscapeRoutes || routes[0].Route != "system-units" ||
		routes[len(routes)-1] != (EscapeRoute{Route: "more", Item: "11 more findings (cron, at)"}) {
		t.Fatalf("bounded report = %d entries: %+v", len(routes), routes)
	}
	if routes := (&routeReport{}).bounded(); routes == nil || len(routes) != 0 {
		t.Fatalf("empty report = %#v", routes)
	}
}

// A directory with too many entries cannot be checked, and a spent
// context stops the scan between checks.
func TestEscapeRouteScanLimits(t *testing.T) {
	f := newEscapeFixture(t)
	for i := range maxDirEntries + 1 {
		f.write(t, "/var/spool/cron/crontabs/e"+strconv.Itoa(i), "")
	}
	f.requireReport(t, "cron: cannot check /var/spool/cron/crontabs (more than 1024 entries)")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	routes := f.s.scan(ctx)
	if len(routes) != 1 || routes[0] != (EscapeRoute{Route: "incomplete", Item: "cannot check the rest of the routes (context canceled)"}) || len(f.r.calls) != 1 {
		t.Fatalf("scan on a spent context = %+v, calls %v", routes, f.r.calls)
	}
	// A spent context also stops a directory walk.
	var report routeReport
	f.s.ctx = ctx
	if entries := f.s.listPolicyDir(&report, "cron", "/etc"); entries != nil || len(report.missed) != 1 || report.missed[0].Route != "incomplete" {
		t.Fatalf("directory walk on a spent context = %v, %+v", entries, report)
	}
}

func TestEscapeProbeDefaults(t *testing.T) {
	p := defaultEscapeProbe()
	if p.verify(-1) != nil {
		t.Fatal("the default verify refuses a descriptor")
	}
	if gids, err := p.processGroups(); err != nil || len(gids) == 0 || gids[0] != uint32(os.Getgid()) {
		t.Fatalf("process groups = %v, %v", gids, err)
	}
	file := filepath.Join(t.TempDir(), "socket")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if acl, err := p.socketACL(file); acl || err != nil {
		t.Fatalf("ACL of a plain file = %v, %v", acl, err)
	}
	if _, err := p.socketACL(file + ".missing"); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("ACL of a missing file = %v", err)
	}
}
