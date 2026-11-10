package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const foreignUID = 65534

// asForeignUser runs create on a locked thread whose file system UID is
// another user, so the kernel records that user as the owner of new sockets.
// It needs root and skips otherwise.
func asForeignUser[T any](t *testing.T, create func() (T, error)) T {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("a socket of another user needs root")
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		// The thread keeps the changed UID, so it ends with this goroutine.
		runtime.LockOSThread()
		if err := unix.Setfsuid(foreignUID); err != nil {
			done <- result{err: err}
			return
		}
		value, err := create()
		done <- result{value, err}
	}()
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	return got.value
}

// actAsUser makes the proxy believe that it runs as uid.
func actAsUser(t *testing.T, uid int) {
	t.Helper()
	saved := effectiveUID
	effectiveUID = func() int { return uid }
	t.Cleanup(func() { effectiveUID = saved })
}

func shortAcceptWait(t *testing.T) {
	t.Helper()
	saved := peerAcceptWait
	peerAcceptWait = 50 * time.Millisecond
	t.Cleanup(func() { peerAcceptWait = saved })
}

func loopbackListener(t *testing.T, network, address string) net.Listener {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Skipf("no %s loopback: %v", network, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func acceptOne(t *testing.T, listener net.Listener, dial func() (net.Conn, error)) (net.Conn, []error) {
	t.Helper()
	var rejected []error
	owner := OwnerListener(listener, func(err error) {
		rejected = append(rejected, err)
		_ = listener.Close()
	})
	client, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	conn, err := owner.Accept()
	if conn != nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	if conn == nil && len(rejected) == 0 {
		t.Fatalf("accept failed without a rejection: %v", err)
	}
	return conn, rejected
}

func TestOwnerListenerAcceptsClientsOfTheProxyUser(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(address, func(t *testing.T) {
			listener := loopbackListener(t, "tcp", address)
			conn, rejected := acceptOne(t, listener, func() (net.Conn, error) { return net.Dial("tcp", listener.Addr().String()) })
			if conn == nil || len(rejected) != 0 {
				t.Fatalf("same-user client rejected: %v", rejected)
			}
		})
	}
}

func TestOwnerListenerRejectsClientsOfOtherUsers(t *testing.T) {
	listener := loopbackListener(t, "tcp", "127.0.0.1:0")
	actAsUser(t, os.Geteuid()+1)
	conn, rejected := acceptOne(t, listener, func() (net.Conn, error) { return net.Dial("tcp", listener.Addr().String()) })
	if conn != nil || len(rejected) != 1 || !errors.Is(rejected[0], ErrForeignPeer) {
		t.Fatalf("foreign client accepted: conn=%v rejected=%v", conn, rejected)
	}
}

func TestOwnerListenerRejectsARealSocketOfAnotherUser(t *testing.T) {
	listener := loopbackListener(t, "tcp", "127.0.0.1:0")
	conn, rejected := acceptOne(t, listener, func() (net.Conn, error) {
		return asForeignUser(t, func() (net.Conn, error) { return net.Dial("tcp", listener.Addr().String()) }), nil
	})
	if conn != nil || len(rejected) != 1 || !strings.Contains(rejected[0].Error(), "65534") {
		t.Fatalf("other user's client accepted: conn=%v rejected=%v", conn, rejected)
	}
}

// A request from another user must not reach the handler: trusted ingress has
// no other check before execution and model metadata routes.
func TestOwnerListenerKeepsForeignRequestsFromTheHandler(t *testing.T) {
	listener := loopbackListener(t, "tcp", "127.0.0.1:0")
	reached := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached <- struct{}{} }), ReadHeaderTimeout: time.Second}
	// Restore the hook only after Serve returns.
	actAsUser(t, os.Geteuid()+1)
	served := make(chan struct{})
	go func() { defer close(served); _ = server.Serve(OwnerListener(listener, func(error) {})) }()
	t.Cleanup(func() { _ = server.Close(); <-served })
	client := &http.Client{Timeout: 2 * time.Second}
	if response, err := client.Get("http://" + listener.Addr().String() + "/v1/models"); err == nil {
		_ = response.Body.Close()
		t.Fatalf("foreign client got status %d", response.StatusCode)
	}
	select {
	case <-reached:
		t.Fatal("foreign request reached the handler")
	default:
	}
}

func TestOwnerListenerReturnsAcceptErrors(t *testing.T) {
	listener := loopbackListener(t, "tcp", "127.0.0.1:0")
	_ = listener.Close()
	if _, err := OwnerListener(listener, func(error) {}).Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("accept error = %v", err)
	}
}

func TestCheckPeerOwnerRequiresLoopbackTCP(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	if err := checkPeerOwner(server); !errors.Is(err, ErrForeignPeer) {
		t.Fatalf("pipe accepted: %v", err)
	}
}

func TestSocketOwnerReportsUnreadableAndMissingTables(t *testing.T) {
	saved := procNet
	t.Cleanup(func() { procNet = saved })
	address := netip.MustParseAddrPort("127.0.0.1:9000")
	procNet = filepath.Join(t.TempDir(), "missing")
	if _, err := socketOwner(address, address); err == nil || !strings.Contains(err.Error(), "read tcp") {
		t.Fatalf("missing tcp table: %v", err)
	}
	procNet = t.TempDir()
	if err := os.WriteFile(filepath.Join(procNet, "tcp"), []byte("header\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := socketOwner(address, address); !errors.Is(err, ErrForeignPeer) || !strings.Contains(err.Error(), "no socket") {
		t.Fatalf("absent socket: %v", err)
	}
	if err := os.Mkdir(filepath.Join(procNet, "tcp6"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := socketOwner(address, address); err == nil || !strings.Contains(err.Error(), "read tcp6") {
		t.Fatalf("unreadable tcp6 table: %v", err)
	}
}

func TestProcAddressMatchesKernelFormat(t *testing.T) {
	for _, test := range []struct{ table, address, want string }{
		{"tcp", "127.0.0.1:8000", "0100007F:1F40"},
		{"tcp6", "[::1]:8000", "00000000000000000000000001000000:1F40"},
		{"tcp6", "127.0.0.1:8000", "0000000000000000FFFF00000100007F:1F40"},
		{"tcp", "[::1]:8000", ""},
	} {
		if got := procAddress(test.table, netip.MustParseAddrPort(test.address)); got != test.want {
			t.Fatalf("%s %s = %q, want %q", test.table, test.address, got, test.want)
		}
	}
}

func dialOwned(t *testing.T, address string) (net.Conn, error) {
	t.Helper()
	dial := ownedPeerDial((&net.Dialer{}).DialContext)
	conn, err := dial(context.Background(), "tcp", address)
	if conn != nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	return conn, err
}

func TestOwnedPeerDialKeepsConnectionsToTheProxyUser(t *testing.T) {
	listener := loopbackListener(t, "tcp", "127.0.0.1:0")
	if _, err := dialOwned(t, listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
}

// The check runs on the dialed connection itself, so a listener that another
// user holds when the connection is made cannot receive the key.
func TestOwnedPeerDialRefusesAListenerOfAnotherUser(t *testing.T) {
	shortAcceptWait(t)
	listener := asForeignUser(t, func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") })
	t.Cleanup(func() { _ = listener.Close() })
	if _, err := dialOwned(t, listener.Addr().String()); !errors.Is(err, errUpstreamKey) || !errors.Is(err, ErrForeignPeer) {
		t.Fatalf("dial to another user's listener = %v", err)
	}
}

func TestOwnedPeerDialRefusesAForeignPeer(t *testing.T) {
	shortAcceptWait(t)
	listener := loopbackListener(t, "tcp", "127.0.0.1:0")
	actAsUser(t, os.Geteuid()+1)
	if _, err := dialOwned(t, listener.Addr().String()); !errors.Is(err, errUpstreamKey) || !errors.Is(err, ErrForeignPeer) {
		t.Fatalf("dial to a foreign peer = %v", err)
	}
}

func TestOwnedPeerDialStopsWaitingWhenCanceled(t *testing.T) {
	listener := loopbackListener(t, "tcp", "127.0.0.1:0")
	actAsUser(t, os.Geteuid()+1)
	ctx, cancel := context.WithCancel(context.Background())
	dial := ownedPeerDial(func(context.Context, string, string) (net.Conn, error) {
		conn, err := net.Dial("tcp", listener.Addr().String())
		cancel()
		return conn, err
	})
	if _, err := dial(ctx, "tcp", listener.Addr().String()); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrForeignPeer) {
		t.Fatalf("canceled dial = %v", err)
	}
}

func TestOwnedPeerDialReturnsDialErrors(t *testing.T) {
	want := errors.New("dial failed")
	dial := ownedPeerDial(func(context.Context, string, string) (net.Conn, error) { return nil, want })
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:1"); !errors.Is(err, want) {
		t.Fatalf("dial error = %v", err)
	}
}

// End to end: a native binding sends no key when another user holds the
// upstream connection, and the client gets 502.
func TestNativeProxySendsNoKeyToAForeignPeer(t *testing.T) {
	shortAcceptWait(t)
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"data":[{"id":"selected"}]}`)
	}))
	defer upstream.Close()
	h := nativeKeyHandler(t, upstream.URL, proxyKeyFile(t))
	actAsUser(t, os.Geteuid()+1)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if response.Code != http.StatusBadGateway || len(seen) != 0 {
		t.Fatalf("status %d, upstream saw %q", response.Code, seen)
	}
}
