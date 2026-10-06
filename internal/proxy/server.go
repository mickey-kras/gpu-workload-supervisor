package proxy

import (
	"bufio"
	"context"
	"errors"
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
		http.Error(w, "proxy is shutting down", http.StatusServiceUnavailable)
		return
	case admissionFull:
		capacityUnavailable(w)
		return
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("{\"error\":\"proxy_capacity_exceeded\"}\n"))
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
	<-drained
	return err
}
