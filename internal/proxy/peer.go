package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrForeignPeer tells that a loopback TCP peer belongs to another user.
var ErrForeignPeer = errors.New("the loopback peer is not owned by the proxy user")

// Test hooks.
var (
	procNet      = "/proc/net"
	effectiveUID = os.Geteuid
	// peerAcceptWait bounds the wait for an upstream to accept a connection.
	// Some kernels show UID 0 in /proc/net/tcp for a socket that the server
	// has not accepted yet.
	peerAcceptWait = time.Second
)

const peerPollInterval = 5 * time.Millisecond

// checkPeerOwner requires that the current user owns the socket at the other
// end of the loopback TCP connection conn. The kernel table identifies that
// socket by the exact address pair of conn, so the check applies to this
// connection and to no other socket.
func checkPeerOwner(conn net.Conn) error {
	local, remote, err := connAddresses(conn)
	if err != nil {
		return err
	}
	owner, err := socketOwner(remote, local)
	if err != nil {
		return err
	}
	if owner != strconv.Itoa(effectiveUID()) {
		return fmt.Errorf("%w: peer %s belongs to user %s", ErrForeignPeer, remote, owner)
	}
	return nil
}

func connAddresses(conn net.Conn) (netip.AddrPort, netip.AddrPort, error) {
	local, localOK := conn.LocalAddr().(*net.TCPAddr)
	remote, remoteOK := conn.RemoteAddr().(*net.TCPAddr)
	if !localOK || !remoteOK || !remote.IP.IsLoopback() {
		return netip.AddrPort{}, netip.AddrPort{}, fmt.Errorf("%w: the connection is not loopback TCP", ErrForeignPeer)
	}
	return local.AddrPort(), remote.AddrPort(), nil
}

// socketOwner returns the UID column of the TCP socket bound to local and
// connected to remote. A dual-stack socket shows an IPv4 address in its
// IPv4-mapped IPv6 form in /proc/net/tcp6.
func socketOwner(local, remote netip.AddrPort) (string, error) {
	for _, table := range []string{"tcp", "tcp6"} {
		owner, found, err := tableSocketOwner(table, procAddress(table, local), procAddress(table, remote))
		if err != nil || found {
			return owner, err
		}
	}
	return "", fmt.Errorf("%w: no socket connects %s to %s", ErrForeignPeer, local, remote)
}

// procAddress formats an address as /proc/net/tcp shows it: each 32-bit word
// of the address in host byte order, then the port.
func procAddress(table string, address netip.AddrPort) string {
	ip := address.Addr().Unmap()
	var raw []byte
	if table == "tcp" {
		if !ip.Is4() {
			return ""
		}
		raw = ip.AsSlice()
	} else {
		bytes := ip.As16()
		raw = bytes[:]
	}
	var text strings.Builder
	for word := 0; word < len(raw); word += 4 {
		fmt.Fprintf(&text, "%08X", binary.NativeEndian.Uint32(raw[word:word+4]))
	}
	fmt.Fprintf(&text, ":%04X", address.Port())
	return text.String()
}

func tableSocketOwner(table, local, remote string) (string, bool, error) {
	if local == "" {
		return "", false, nil
	}
	file, err := os.Open(procNet + "/" + table)
	if errors.Is(err, os.ErrNotExist) && table == "tcp6" {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: read %s: %w", ErrForeignPeer, table, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Scan() // header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 8 && fields[1] == local && fields[2] == remote {
			return fields[7], true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("%w: read %s: %w", ErrForeignPeer, table, err)
	}
	return "", false, nil
}

// OwnerListener accepts only loopback TCP connections whose client socket the
// proxy user owns. It closes other connections before they reach HTTP and
// reports each one to reject.
func OwnerListener(listener net.Listener, reject func(error)) net.Listener {
	return ownerListener{Listener: listener, reject: reject}
}

type ownerListener struct {
	net.Listener
	reject func(error)
}

func (l ownerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		err = checkPeerOwner(conn)
		if err == nil {
			return conn, nil
		}
		l.reject(errors.Join(err, conn.Close()))
	}
}

type dialFunc func(context.Context, string, string) (net.Conn, error)

// ownedPeerDial keeps a new upstream connection only when the current user
// owns the socket that accepted it. The key is sent on this connection, so a
// listener change after the check cannot receive it.
func ownedPeerDial(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if err := waitPeerOwner(ctx, conn); err != nil {
			return nil, errors.Join(fmt.Errorf("%w: %w", errUpstreamKey, err), conn.Close())
		}
		return conn, nil
	}
}

func waitPeerOwner(ctx context.Context, conn net.Conn) error {
	deadline := time.Now().Add(peerAcceptWait)
	for {
		err := checkPeerOwner(conn)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(peerPollInterval):
		}
	}
}
