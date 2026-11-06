package runtime

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// escapeAccount is the service account of the escape route checks.
type escapeAccount struct {
	name string
	uid  int
	// gids holds each group of the account: the primary group, each
	// /etc/group entry that lists the account and each group of this
	// process.
	gids map[uint32]bool
	// groups maps each /etc/group name to its group ID.
	groups map[string]uint32
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

// lookupAccount reads the name and the groups of the service UID from
// /etc/passwd and /etc/group. The cron, at, group and sudo checks need the
// name, so an account that setup cannot read opens a route.
func (m *SystemdManager) lookupAccount(scan *routeScan) (escapeAccount, bool) {
	account := escapeAccount{uid: m.host.uid, gids: map[uint32]bool{}, groups: map[string]uint32{}}
	passwd, ok := m.readRoutePolicy(scan, "account", "/etc/passwd", false)
	if !ok {
		return account, false
	}
	name, gid, found := passwdEntry(string(passwd), m.host.uid)
	if !found {
		scan.add("account", fmt.Sprintf("/etc/passwd has no entry for uid %d, so setup cannot check the routes of its name", m.host.uid))
		return account, false
	}
	account.name = name
	account.gids[gid] = true
	// A failed group read records its route. The checks of the name still
	// run, so the error names each route.
	group, ok := m.readRoutePolicy(scan, "groups", "/etc/group", false)
	if !ok {
		return account, true
	}
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
		if slices.Contains(strings.Split(fields[3], ","), name) {
			account.gids[gid] = true
		}
	}
	gids, err := m.escape.processGroups()
	if err != nil {
		scan.fail("groups", err)
	}
	for _, gid := range gids {
		account.gids[gid] = true
	}
	return account, true
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

// checkGroups names each risky group of the account and each daemon
// socket that the account can open.
func (m *SystemdManager) checkGroups(scan *routeScan, account escapeAccount) {
	for _, name := range riskyGroups {
		if gid, ok := account.groups[name]; ok && account.gids[gid] {
			scan.add("groups", "member of "+name)
		}
	}
	for _, socket := range daemonSockets {
		m.checkSocket(scan, account, socket)
	}
}

// checkSocket names a daemon socket that the account can open: its mode
// lets the account write, or it has an ACL. A socket that the account
// cannot reach is closed.
func (m *SystemdManager) checkSocket(scan *routeScan, account escapeAccount, socket string) {
	name := m.host.path(socket)
	var stat unix.Stat_t
	err := unix.Stat(name, &stat)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOTDIR) {
		return
	}
	if err != nil {
		scan.fail("sockets", fmt.Errorf("%s: %w", socket, err))
		return
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return
	}
	if canWrite(stat, m.host.ownerOf(stat), account) {
		scan.add("sockets", socket+" is writable by this account")
		return
	}
	acl, err := m.escape.socketACL(name)
	if err != nil {
		scan.fail("sockets", fmt.Errorf("%s: %w", socket, err))
		return
	}
	if acl {
		scan.add("sockets", socket+" has an ACL")
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
