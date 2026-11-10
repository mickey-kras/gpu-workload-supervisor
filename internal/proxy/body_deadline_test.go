package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type bodyResult struct {
	body string
	err  error
}

// bodyDeadlineServer serves a Server with body limits whose handler reports
// what it read and then answers after a pause longer than both limits.
func bodyDeadlineServer(t *testing.T, total, idle time.Duration) (*httptest.Server, <-chan bodyResult) {
	t.Helper()
	results := make(chan bodyResult, 1)
	tracked := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		results <- bodyResult{string(body), err}
		if err != nil {
			return
		}
		time.Sleep(2 * max(total, idle))
		if r.Context().Err() != nil {
			t.Error("the request context ended after the body")
		}
		_, _ = io.WriteString(w, "done")
	}), 1, 1, DefaultCompletionPath)
	tracked.BodyReadTimeout, tracked.BodyIdleTimeout = total, idle
	server := httptest.NewServer(tracked)
	t.Cleanup(server.Close)
	return server, results
}

// sendBody writes a request with a 10-byte body, one byte for each pause.
func sendBody(t *testing.T, server *httptest.Server, pauses []time.Duration) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "POST /execute HTTP/1.1\r\nHost: proxy\r\nContent-Length: 10\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	for _, pause := range pauses {
		time.Sleep(pause)
		if _, err := io.WriteString(conn, "x"); err != nil {
			break
		}
	}
	return conn
}

func waitBody(t *testing.T, results <-chan bodyResult) bodyResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("the handler is still reading the body")
		return bodyResult{}
	}
}

func TestBodyIdleTimeoutReleasesAStalledBody(t *testing.T) {
	server, results := bodyDeadlineServer(t, time.Minute, 100*time.Millisecond)
	sendBody(t, server, []time.Duration{0})
	if result := waitBody(t, results); !os.IsTimeout(result.err) {
		t.Fatalf("stalled body result = %+v", result)
	}
}

func TestBodyReadTimeoutReleasesATrickleBody(t *testing.T) {
	server, results := bodyDeadlineServer(t, 150*time.Millisecond, time.Second)
	pauses := make([]time.Duration, 10)
	for i := range pauses {
		pauses[i] = 40 * time.Millisecond
	}
	sendBody(t, server, pauses)
	if result := waitBody(t, results); !os.IsTimeout(result.err) {
		t.Fatalf("trickle body result = %+v", result)
	}
}

// The deadline covers only the body: a response written after it passes,
// as a stream does, still reaches the client.
func TestBodyDeadlinesLeaveTheResponseOpen(t *testing.T) {
	server, results := bodyDeadlineServer(t, 200*time.Millisecond, 100*time.Millisecond)
	conn := sendBody(t, server, make([]time.Duration, 10))
	if result := waitBody(t, results); result.err != nil || result.body != strings.Repeat("x", 10) {
		t.Fatalf("complete body result = %+v", result)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "done" {
		t.Fatalf("response %d %q: %v", response.StatusCode, body, err)
	}
}

func TestBodyDeadlineFailsWithoutConnectionControl(t *testing.T) {
	var readErr error
	tracked := NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}), 1, 1, DefaultCompletionPath)
	tracked.BodyIdleTimeout = time.Second
	tracked.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/execute", strings.NewReader("body")))
	if readErr == nil || !strings.Contains(readErr.Error(), "set request body deadline") {
		t.Fatalf("read without deadline support = %v", readErr)
	}
}

func TestBodyDeadlinesLeaveForwardedResponsesOpen(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "delayed"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "body" {
					t.Errorf("forwarded body = %q: %v", body, err)
					return
				}
				if streaming {
					_, _ = io.WriteString(w, "start\n")
					w.(http.Flusher).Flush()
				}
				select {
				case <-time.After(400 * time.Millisecond):
				case <-r.Context().Done():
					t.Error("upstream request canceled after the body")
					return
				}
				_, _ = io.WriteString(w, "done")
			}))
			defer upstream.Close()
			target, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			tracked := NewServer(httputil.NewSingleHostReverseProxy(target), 1, 1, DefaultCompletionPath)
			tracked.BodyReadTimeout = 200 * time.Millisecond
			tracked.BodyIdleTimeout = 100 * time.Millisecond
			server := httptest.NewServer(tracked)
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Post(server.URL+"/execute", "text/plain", strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			want := "done"
			if streaming {
				want = "start\ndone"
			}
			if err != nil || response.StatusCode != http.StatusOK || string(body) != want {
				t.Fatalf("response %d %q: %v", response.StatusCode, body, err)
			}
		})
	}
}
