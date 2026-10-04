package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// ReadRequest consumes one newline-terminated JSON document and EOF. A client
// must close its input after the request; trailing documents are never replayed.
func ReadRequest(r io.Reader, budget time.Duration) ([]byte, error) {
	type result struct {
		b []byte
		e error
	}
	done := make(chan result, 1)
	go func() { b, e := io.ReadAll(io.LimitReader(r, MaxRequestBytes+1)); done <- result{b, e} }()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case x := <-done:
		if x.e != nil {
			return nil, x.e
		}
		if len(x.b) > MaxRequestBytes || len(x.b) == 0 || x.b[len(x.b)-1] != '\n' || bytes.Count(x.b, []byte("\n")) != 1 {
			return nil, errors.New("invalid request framing")
		}
		return x.b, nil
	case <-timer.C:
		if c, ok := r.(io.Closer); ok {
			c.Close()
		}
		return nil, ErrIOTimeout
	}
}

var ErrIOTimeout = errors.New("operator I/O deadline exceeded")

// WriteResponse runs only after the operation has finished and persisted. A
// failed/blocked reader cannot cancel or replay any backend operation.
func WriteResponse(w io.Writer, r Response, budget time.Duration) error {
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b)+1 > MaxResponseBytes {
		return errors.New("operator response exceeds limit")
	}
	b = append(b, '\n')
	done := make(chan error, 1)
	go func() {
		n, e := w.Write(b)
		if e == nil && n != len(b) {
			e = io.ErrShortWrite
		}
		done <- e
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case e := <-done:
		return e
	case <-timer.C:
		if c, ok := w.(io.Closer); ok {
			c.Close()
		}
		return ErrIOTimeout
	}
}
