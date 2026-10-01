package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

func TestHTTPRequestErrorsDoNotExposeEndpointSecrets(t *testing.T) {
	const endpoint = "http://sentinel-user:sentinel-password@127.0.0.1/health?token=sentinel-query"
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	tests := []struct {
		name      string
		transport responseTransport
		want      string
		cause     error
	}{
		{"transport", responseTransport{err: refused}, "network failure", syscall.ECONNREFUSED},
		{"canceled", responseTransport{err: context.Canceled}, "canceled", context.Canceled},
		{"timeout", responseTransport{err: context.DeadlineExceeded}, "deadline exceeded", context.DeadlineExceeded},
		{"nested URL", responseTransport{err: fmt.Errorf("proxy %s: %w", endpoint, &url.Error{Op: "Get", URL: endpoint, Err: refused})}, "network failure", syscall.ECONNREFUSED},
		{"malformed redirect", responseTransport{response: &http.Response{StatusCode: 302, Header: http.Header{"Location": {strings.Replace(endpoint, "/health", "/%zz", 1)}}, Body: io.NopCloser(strings.NewReader(""))}}, "HTTP request failed", nil},
		{"opaque transport", responseTransport{err: errors.New("failed for " + endpoint)}, "HTTP request failed", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := &SystemdManager{client: &http.Client{Transport: tt.transport}}
			err = manager.checkHTTPResponse(req, "text health")
			if err == nil {
				t.Fatal("expected request failure")
			}
			for _, secret := range []string{"sentinel-user", "sentinel-password", "sentinel-query"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks %s: %v", secret, err)
				}
			}
			if !strings.Contains(err.Error(), "text health request") || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("missing safe diagnostic %q: %v", tt.want, err)
			}
			var urlError *url.Error
			if !errors.As(err, &urlError) {
				t.Errorf("lost URL error identity: %v", err)
			}
			if tt.cause != nil && !errors.Is(err, tt.cause) {
				t.Errorf("lost cause %v: %v", tt.cause, err)
			}
		})
	}
}
