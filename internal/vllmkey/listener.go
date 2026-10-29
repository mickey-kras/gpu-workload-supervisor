package vllmkey

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// ErrForeignListener tells that a process of another user can receive the
// connection to the vLLM port. The key is not sent to it.
var ErrForeignListener = errors.New("the vLLM port is not held only by the desktop user")

// Test hook.
var procNet = "/proc/net"

// tcpListenState is the LISTEN state in /proc/net/tcp.
const tcpListenState = "0A"

// Local addresses whose listener receives a connection to 127.0.0.1: the
// IPv4 loopback and wildcard addresses, and the IPv6 wildcard and mapped
// loopback addresses (dual-stack sockets).
var loopbackListeners = map[string]map[string]bool{
	"tcp":  {"0100007F": true, "00000000": true},
	"tcp6": {strings.Repeat("0", 32): true, "0000000000000000FFFF00000100007F": true},
}

// CheckListener requires that every socket that listens for connections to
// 127.0.0.1 on the port of endpoint belongs to the current user, and that at
// least one does. It reads /proc/net/tcp and /proc/net/tcp6.
func CheckListener(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() != "127.0.0.1" {
		return fmt.Errorf("%w: endpoint must be http://127.0.0.1:<port>", ErrForeignListener)
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("%w: endpoint has no valid port", ErrForeignListener)
	}
	wantPort := fmt.Sprintf("%04X", port)
	uid := strconv.Itoa(effectiveUID())
	found := false
	for _, table := range []string{"tcp", "tcp6"} {
		owners, err := listenerOwners(table, wantPort)
		if err != nil {
			return err
		}
		for _, owner := range owners {
			if owner != uid {
				return fmt.Errorf("%w: user %s also listens on port %d", ErrForeignListener, owner, port)
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%w: nothing listens on port %d", ErrForeignListener, port)
	}
	return nil
}

func listenerOwners(table, port string) ([]string, error) {
	file, err := os.Open(procNet + "/" + table)
	if errors.Is(err, os.ErrNotExist) && table == "tcp6" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrForeignListener, table, err)
	}
	defer file.Close()
	var owners []string
	scanner := bufio.NewScanner(file)
	scanner.Scan() // header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[3] != tcpListenState {
			continue
		}
		address, localPort, ok := strings.Cut(fields[1], ":")
		if ok && localPort == port && loopbackListeners[table][address] {
			owners = append(owners, fields[7])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrForeignListener, table, err)
	}
	return owners, nil
}
