package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// escapeAccount is the service account of the escape route scan.
type escapeAccount struct {
	// name is empty when /etc/passwd has no entry for uid.
	name string
	uid  int
	// gids holds each group of the account: the primary group, each
	// /etc/group entry that lists the account and each group of this
	// process.
	gids map[uint32]bool
	// groups maps each /etc/group name to its group ID.
	groups map[string]uint32
}

// is reports a user field that names the account, by name or by number.
func (a escapeAccount) is(user string) bool {
	return user != "" && (user == a.name || user == strconv.Itoa(a.uid))
}

// riskyGroups give root-equivalent control or raw disk access, so a
// member can run programs outside host protection.
var riskyGroups = []string{"docker", "podman", "libvirt", "lxd", "incus", "sudo", "wheel", "disk"}

// daemonSockets are the sockets of daemons that run programs for a client
// outside the slice of the client.
var daemonSockets = []string{
	"/run/docker.sock",
	"/run/podman/podman.sock",
	"/run/containerd/containerd.sock",
	"/run/libvirt/libvirt-sock",
	"/run/libvirt/virtqemud-sock",
	"/var/lib/lxd/unix.socket",
	"/var/snap/lxd/common/lxd/unix.socket",
	"/var/lib/incus/unix.socket",
}

// lookupAccount reads the name and the groups of the account from
// /etc/passwd, /etc/group and this process. The name stays empty when the
// scan cannot find it: the cron and at checks and the member lists of
// /etc/group need it, so the report then says that it cannot check them.
// The process groups and /etc/group still give the group names.
func (s escapeScanner) lookupAccount(report *routeReport) escapeAccount {
	account := escapeAccount{uid: s.host.uid, gids: map[uint32]bool{}, groups: map[string]uint32{}}
	gids, err := s.probe.processGroups()
	if err != nil {
		report.unchecked(routeGroups, "the groups of this process ("+err.Error()+")")
	}
	for _, gid := range gids {
		account.gids[gid] = true
	}
	reason := "/etc/passwd cannot be read"
	if passwd, ok := s.readRoutePolicy(report, routeAccount, "/etc/passwd", false); ok {
		reason = fmt.Sprintf("/etc/passwd has no entry for uid %d", s.host.uid)
		if name, gid, found := passwdEntry(string(passwd), s.host.uid); found {
			account.name = name
			account.gids[gid] = true
		}
	}
	if account.name == "" {
		report.unchecked(routeAccount, "cron, at and the member lists of /etc/group: "+reason)
	}
	group, _ := s.readRoutePolicy(report, routeGroups, "/etc/group", false)
	for _, line := range strings.Split(string(group), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 4 {
			continue
		}
		gid, ok := parseID(fields[2])
		if !ok {
			continue
		}
		account.groups[fields[0]] = gid
		if account.name != "" && slices.Contains(strings.Split(fields[3], ","), account.name) {
			account.gids[gid] = true
		}
	}
	return account
}

// parseID parses a user or group ID.
func parseID(field string) (uint32, bool) {
	v, err := strconv.ParseUint(field, 10, 32)
	return uint32(v), err == nil
}

// passwdEntry finds the name and primary group of uid in /etc/passwd.
func passwdEntry(passwd string, uid int) (string, uint32, bool) {
	for _, line := range strings.Split(passwd, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 || fields[2] != strconv.Itoa(uid) {
			continue
		}
		if gid, ok := parseID(fields[3]); ok && fields[0] != "" {
			return fields[0], gid, true
		}
	}
	return "", 0, false
}

// checkGroups reports each risky group of the account and each daemon
// socket that the account can open.
func (s escapeScanner) checkGroups(report *routeReport, account escapeAccount) {
	for _, name := range riskyGroups {
		if gid, ok := account.groups[name]; ok && account.gids[gid] {
			report.add(routeGroups, "member of "+name)
		}
	}
	for _, socket := range daemonSockets {
		s.checkSocket(report, account, socket)
	}
}

// checkSocket reports a daemon socket that the account can open: its mode
// lets the account write, or it has an ACL. The check follows a symlink to
// the socket. A socket path that the account cannot reach is closed.
func (s escapeScanner) checkSocket(report *routeReport, account escapeAccount, socket string) {
	name := s.host.path(socket)
	var stat unix.Stat_t
	err := unix.Stat(name, &stat)
	if policyAbsent(err) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOTDIR) {
		return
	}
	if err != nil {
		report.unchecked(routeSockets, socket+" ("+err.Error()+")")
		return
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return
	}
	if canWrite(stat, s.host.ownerOf(stat), account) {
		report.add(routeSockets, socket+" is writable by this account")
		return
	}
	acl, err := s.probe.socketACL(name)
	switch {
	case err != nil:
		report.unchecked(routeSockets, socket+" ("+err.Error()+")")
	case acl:
		report.add(routeSockets, socket+" has an ACL")
	}
}

// canWrite applies the mode bits as the kernel does: the owner class, then
// the group class, then the other class.
func canWrite(stat unix.Stat_t, owner uint32, account escapeAccount) bool {
	switch {
	case owner == uint32(account.uid):
		return stat.Mode&0o200 != 0
	case account.gids[stat.Gid]:
		return stat.Mode&0o020 != 0
	default:
		return stat.Mode&0o002 != 0
	}
}
