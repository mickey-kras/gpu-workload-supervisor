package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestAdapterScrubsReflectedSecretsFromResponseMetadata(t *testing.T) {
	secrets := []string{"test-registration-token", executionCredential, completionCredential}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(DefaultRegistrationTokenHeader) != secrets[0] {
			t.Error("upstream correlation lost")
		}
		for _, secret := range secrets {
			for _, reflected := range []string{secret, strings.ToUpper(secret), strings.ToUpper(secret[:len(secret)/2]) + secret[len(secret)/2:]} {
				w.Header().Add("X-Debug", "prefix="+reflected+";suffix")
				w.Header().Add("Set-Cookie", "session="+reflected+"; Path=/")
			}
		}
		w.Header().Add("X-Debug", "safe")
		w.Header().Set("X-Debug-"+secrets[0], "secret-in-name")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Trailer", "X-Late-Debug")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "accepted")
		for _, secret := range secrets {
			for _, reflected := range []string{secret, strings.ToUpper(secret), strings.ToUpper(secret[:len(secret)/2]) + secret[len(secret)/2:]} {
				w.Header().Add("X-Late-Debug", "prefix="+reflected+";suffix")
			}
		}
		w.Header().Add("X-Late-Debug", "safe")
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(adapterHandler(t, &fakeStore{state: admittedState(control.OwnerSupervisor)}, upstream.URL))
	defer proxy.Close()
	request, _ := http.NewRequest("POST", proxy.URL+"/execute", nil)
	addLeaseHeaders(request, admittedState(control.OwnerSupervisor).LeaseFence)
	request.Header.Set(ExecutionCredentialHeader, executionCredential)
	hints := 0
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{Got1xxResponse: func(_ int, header textproto.MIMEHeader) error {
		hints++
		assertNoReflectedSecrets(t, http.Header(header), secrets)
		return nil
	}}))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, declared := response.Trailer["X-Late-Debug"]; !declared {
		t.Fatal("safe trailer declaration missing before EOF")
	}
	_, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	assertNoReflectedSecrets(t, response.Header, secrets)
	assertNoReflectedSecrets(t, response.Trailer, secrets)
	if hints != 1 || response.StatusCode != http.StatusAccepted || response.Header.Get("X-Debug") != "safe" || response.Trailer.Get("X-Late-Debug") != "safe" {
		t.Fatal("safe metadata or lifecycle changed")
	}
}

func assertNoReflectedSecrets(t *testing.T, header http.Header, secrets []string) {
	t.Helper()
	for name, values := range header {
		for _, secret := range secrets {
			if strings.Contains(strings.ToLower(name), strings.ToLower(secret)) {
				t.Error("secret reflected in metadata name")
			}
		}
		for _, value := range values {
			for _, secret := range secrets {
				if strings.Contains(strings.ToLower(value), strings.ToLower(secret)) {
					t.Errorf("secret reflected under %s", name)
				}
			}
		}
	}
}

func TestAdapterReflectedSecretFilteringPreservesUpgrade(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, reader, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		token := r.Header.Get(DefaultRegistrationTokenHeader)
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\nX-Debug: prefix=%s;suffix\r\n\r\n", token)
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(conn, "echo:"+line)
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(adapterHandler(t, &fakeStore{state: admittedState(control.OwnerSupervisor)}, upstream.URL))
	defer proxy.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = fmt.Fprintf(conn, "POST /execute HTTP/1.1\r\nHost: proxy\r\nConnection: Upgrade\r\nUpgrade: test\r\n%s: %s\r\n%s: request-1\r\n%s: %s\r\n%s: 7\r\n\r\n", ExecutionCredentialHeader, executionCredential, DefaultRequestIDHeader, DefaultFenceIDHeader, admittedState(control.OwnerSupervisor).LeaseFence.Incarnation, DefaultFenceEpochHeader)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("X-Debug") != "" {
		t.Fatal("upgrade reflection or handshake failure")
	}
	_, _ = io.WriteString(conn, "hello\n")
	line, err := reader.ReadString('\n')
	if err != nil || line != "echo:hello\n" {
		t.Fatalf("upgrade data %q: %v", line, err)
	}
}

func TestTrustedIngressPreservesReflectedMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Debug", "prefix="+r.Header.Get(DefaultRegistrationTokenHeader)+";suffix")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	handler := testHandler(t, &fakeStore{state: admittedState(control.OwnerSupervisor)}, upstream.URL)
	request := httptest.NewRequest("POST", "/execute", nil)
	addLeaseHeaders(request, admittedState(control.OwnerSupervisor).LeaseFence)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("X-Debug") != "prefix=test-registration-token;suffix" {
		t.Fatal("default trusted mode changed")
	}
}
