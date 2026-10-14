package httptransport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDirectTransportBoundsHeaderWait(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		name := "HTTP1"
		if http2 {
			name = "HTTP2"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if http2 && r.ProtoMajor != 2 {
					t.Errorf("protocol = %s, want HTTP/2", r.Proto)
				}
				<-r.Context().Done()
			}))
			upstream.EnableHTTP2 = http2
			upstream.StartTLS()
			defer upstream.Close()
			transport := NewDirect()
			defer transport.CloseIdleConnections()
			if transport.ResponseHeaderTimeout <= 0 {
				t.Fatal("default transport leaves response-header waits unbounded")
			}
			transport.ResponseHeaderTimeout = 50 * time.Millisecond
			transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			client := &http.Client{Transport: transport}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if response != nil {
				response.Body.Close()
			}
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() || ctx.Err() != nil {
				t.Fatalf("header wait error = %v, request context = %v", err, ctx.Err())
			}
		})
	}
}

func TestDirectTransportIgnoresAmbientProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("trusted endpoint traffic reached ambient proxy")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	endpoint, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := NewDirect()
	defer transport.CloseIdleConnections()
	// Supply local DNS for a non-loopback name so Go's localhost proxy exemption
	// cannot make this pass when ProxyFromEnvironment is accidentally enabled.
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "runtime.example:80" {
			address = endpoint.Host
		}
		return dial(ctx, network, address)
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("http://runtime.example/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want direct upstream 202", response.StatusCode)
	}
}

func TestDirectTransportStreamsBeforeCompletionAndCancels(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: started\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	transport := NewDirect()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len("data: started\n\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	if string(first) != "data: started\n\n" {
		t.Fatalf("first stream event = %q", first)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not observe stream cancellation")
	}
}

func TestDirectTransportReusesBurstConnections(t *testing.T) {
	const parallel = 16
	releases := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	arrivals := [2]chan struct{}{make(chan struct{}, parallel), make(chan struct{}, parallel)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wave := 0
		if r.URL.Path == "/second" {
			wave = 1
		}
		arrivals[wave] <- struct{}{}
		<-releases[wave]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	transport := NewDirect()
	defer transport.CloseIdleConnections()
	dial := transport.DialContext
	var dials atomic.Int64
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err == nil {
			dials.Add(1)
		}
		return conn, err
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	runBurst := func(wave int) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, parallel)
		for i := 0; i < parallel; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				url := upstream.URL
				if wave == 1 {
					url += "/second"
				}
				response, err := client.Get(url)
				if err != nil {
					errs <- err
					return
				}
				_, err = io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if err != nil {
					errs <- err
				} else if closeErr != nil {
					errs <- closeErr
				}
			}()
		}
		for i := 0; i < parallel; i++ {
			select {
			case <-arrivals[wave]:
			case <-time.After(5 * time.Second):
				close(releases[wave])
				t.Fatal("upstream did not receive full burst")
			}
		}
		close(releases[wave])
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
	}
	runBurst(0)
	if got := dials.Load(); got != parallel {
		t.Fatalf("warm burst dials = %d, want %d", got, parallel)
	}
	runBurst(1)
	if got := dials.Load(); got != parallel {
		t.Fatalf("reused burst dials = %d, want %d", got, parallel)
	}
}
