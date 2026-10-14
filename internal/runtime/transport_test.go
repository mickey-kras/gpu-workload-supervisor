package runtime

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestSystemdManagerOwnsDirectTransport(t *testing.T) {
	manager, err := NewSystemdManager(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := manager.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("health probes use %T transport, want private *http.Transport", manager.client.Transport)
	}
	defer transport.CloseIdleConnections()
	if transport == http.DefaultTransport || transport.Proxy != nil {
		t.Fatal("health probes inherit global transport or ambient proxy")
	}
}

func TestSystemdManagerPreservesConfiguredHealthBudget(t *testing.T) {
	for _, test := range []struct {
		name     string
		delay    time.Duration
		wantErr  error
		wantWait time.Duration
	}{
		{"slow successful headers", 11 * time.Minute, nil, 11 * time.Minute},
		{"health budget expires", 21 * time.Minute, context.DeadlineExceeded, 20 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				config := testConfig()
				config.HealthTimeout = 20 * time.Minute
				manager, err := NewSystemdManager(config)
				if err != nil {
					t.Fatal(err)
				}
				transport := manager.client.Transport.(*http.Transport)
				defer transport.CloseIdleConnections()
				// net.Pipe keeps HTTP I/O inside the virtual-time bubble while
				// exercising the constructor's real transport and client deadlines.
				client, upstream := net.Pipe()
				defer client.Close()
				defer upstream.Close()
				transport.DialContext = func(context.Context, string, string) (net.Conn, error) { return client, nil }
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() {
					request, err := http.ReadRequest(bufio.NewReader(upstream))
					if err != nil {
						t.Error(err)
						return
					}
					defer request.Body.Close()
					select {
					case <-time.After(test.delay):
						_, _ = io.WriteString(upstream, "HTTP/1.1 204 No Content\r\n\r\n")
					case <-ctx.Done():
					}
				}()
				started := time.Now()
				err = manager.getHealthy(ctx, "http://127.0.0.1/health")
				if !errors.Is(err, test.wantErr) || time.Since(started) != test.wantWait {
					t.Fatalf("health response = %v after %v, want %v after %v", err, time.Since(started), test.wantErr, test.wantWait)
				}
			})
		})
	}
}
