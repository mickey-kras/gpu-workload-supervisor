package runtime

import (
	"net/http"
	"testing"
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
