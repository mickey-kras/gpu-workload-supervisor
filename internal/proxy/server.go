package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server prevents new proxy work after shutdown begins and reports when
// every admitted request has left its handler, even if Shutdown times out.
type Server struct {
	handler         http.Handler
	mu              sync.Mutex
	active          int
	ordinary        int
	completion      int
	maxOrdinary     int
	maxCompletion   int
	completionPath  string
	stopping        bool
	drained         chan struct{}
	hijacked        map[net.Conn]struct{}
	closingHijacked bool
	// BodyReadTimeout bounds the time from the first read of a request body to
	// its end. BodyIdleTimeout bounds each wait for more body bytes. Zero
	// disables a limit. Response streams have no deadline.
	BodyReadTimeout time.Duration
	BodyIdleTimeout time.Duration
}

func NewServer(handler http.Handler, maxOrdinary, maxCompletion int, completionPath string) *Server {
	if completionPath == "" {
		completionPath = DefaultCompletionPath
	}
	return &Server{handler: handler, maxOrdinary: maxOrdinary, maxCompletion: maxCompletion, completionPath: completionPath}
}

// trackingWriter records connections that leave net/http's ownership on
// Hijack. ReverseProxy uses ResponseController.Hijack for upgrades.
type trackingWriter struct {
	http.ResponseWriter
	owner *Server
	conn  net.Conn
}

func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.owner.mu.Lock()
	if w.owner.hijacked == nil {
		w.owner.hijacked = make(map[net.Conn]struct{})
	}
	w.owner.hijacked[conn] = struct{}{}
	w.conn = conn
	closing := w.owner.closingHijacked
	w.owner.mu.Unlock()
	if closing {
		_ = conn.Close()
	}
	return conn, rw, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	isCompletion := r.Method == http.MethodPost && r.URL.Path == s.completionPath
	switch s.admit(isCompletion) {
	case admissionStopping:
		writeError(w, http.StatusServiceUnavailable, "proxy_shutting_down")
		return
	case admissionFull:
		capacityUnavailable(w)
		return
	}
	if (s.BodyReadTimeout > 0 || s.BodyIdleTimeout > 0) && r.Body != nil && r.Body != http.NoBody {
		r.Body = &deadlineBody{ReadCloser: r.Body, controller: http.NewResponseController(w), total: s.BodyReadTimeout, idle: s.BodyIdleTimeout}
	}
	writer := &trackingWriter{ResponseWriter: w, owner: s}
	defer func() {
		s.mu.Lock()
		delete(s.hijacked, writer.conn)
		s.active--
		if isCompletion {
			s.completion--
		} else {
			s.ordinary--
		}
		if s.stopping && s.active == 0 {
			close(s.drained)
		}
		s.mu.Unlock()
	}()
	s.handler.ServeHTTP(writer, r)
}

type admissionResult uint8

const (
	admissionAccepted admissionResult = iota
	admissionStopping
	admissionFull
)

func (s *Server) admit(isCompletion bool) admissionResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return admissionStopping
	}
	if isCompletion {
		if s.maxCompletion > 0 && s.completion >= s.maxCompletion {
			return admissionFull
		}
		s.completion++
	} else {
		if s.maxOrdinary > 0 && s.ordinary >= s.maxOrdinary {
			return admissionFull
		}
		s.ordinary++
	}
	s.active++
	return admissionAccepted
}

func capacityUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, "proxy_capacity_exceeded")
}

func (s *Server) closeHijacked() error {
	s.mu.Lock()
	s.closingHijacked = true
	conns := make([]net.Conn, 0, len(s.hijacked))
	for conn := range s.hijacked {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	var err error
	for _, conn := range conns {
		err = errors.Join(err, conn.Close())
	}
	return err
}

func (s *Server) stop() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopping {
		s.stopping = true
		s.drained = make(chan struct{})
		if s.active == 0 {
			close(s.drained)
		}
	}
	return s.drained
}

const forcedDrainTimeout = 5 * time.Second

func (s *Server) ShutdownAndDrain(server *http.Server, timeout time.Duration) error {
	drained := s.stop()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := server.Shutdown(ctx)
	if err != nil {
		// Shutdown leaves active connections open when its deadline expires.
		// Close cancels them; a handler may still need time to return.
		err = errors.Join(err, server.Close(), s.closeHijacked())
	} else {
		select {
		case <-drained:
			return nil
		case <-ctx.Done():
			err = errors.Join(ctx.Err(), server.Close(), s.closeHijacked())
		}
	}
	select {
	case <-drained:
	case <-time.After(forcedDrainTimeout):
		err = errors.Join(err, errors.New("proxy handlers did not drain after forced close"))
	}
	return err
}

// deadlineBody sets a read deadline on the client connection for each body
// read, so a slow or stalled body cannot hold a handler slot. net/http clears
// the deadline when the body ends, on hijack and before the next request, so
// response streams and upgraded connections keep no deadline.
type deadlineBody struct {
	io.ReadCloser
	controller  *http.ResponseController
	total, idle time.Duration
	end         time.Time
}

func (b *deadlineBody) Read(buffer []byte) (int, error) {
	now := time.Now()
	if b.end.IsZero() && b.total > 0 {
		b.end = now.Add(b.total)
	}
	deadline := b.end
	if b.idle > 0 && (deadline.IsZero() || now.Add(b.idle).Before(deadline)) {
		deadline = now.Add(b.idle)
	}
	if err := b.controller.SetReadDeadline(deadline); err != nil {
		return 0, fmt.Errorf("set request body deadline: %w", err)
	}
	return b.ReadCloser.Read(buffer)
}
