package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// Native bindings observe upstream responses for runtime-specific terminal
// evidence and finish the registration themselves. EOF, errors, truncation
// and disconnects are never proof: without verified terminal evidence the
// work stays unfinished for explicit reconciliation.
const maxObservedResponseBytes = 16 << 20

const observedFinishTimeout = 10 * time.Second

type terminalDetector interface {
	observe(chunk []byte, eof bool) (terminal bool)
}

func (h *Handler) observeNativeTerminal(response *http.Response, path, requestID string, fence control.Fence, token string) {
	if h.nativeModel == nil || response.StatusCode < 200 || response.StatusCode > 299 {
		return
	}
	if response.Header.Get("Content-Encoding") != "" {
		return
	}
	detector := newTerminalDetector(path, response.Header.Get("Content-Type"))
	if detector == nil {
		return
	}
	response.Body = &observedBody{
		body:     response.Body,
		detector: detector,
		onTerminal: func() {
			h.finishObserved(requestID, fence, token)
		},
	}
}

func (h *Handler) finishObserved(requestID string, fence control.Fence, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), observedFinishTimeout)
	defer cancel()
	// A caller callback or reconciliation may have finished the work first.
	// A failed finish leaves durable state unchanged and the work unfinished.
	_ = h.store.FinishWorkToken(ctx, requestID, h.workload, fence, token, store.WorkCompleted)
}

type observedBody struct {
	body       io.ReadCloser
	detector   terminalDetector
	once       sync.Once
	onTerminal func()
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 && b.detector.observe(p[:n], false) {
		b.once.Do(b.onTerminal)
	}
	if err == io.EOF && b.detector.observe(nil, true) {
		b.once.Do(b.onTerminal)
	}
	return n, err
}

func (b *observedBody) Close() error { return b.body.Close() }

func newTerminalDetector(path, contentType string) terminalDetector {
	switch path {
	case "/api/generate", "/api/chat":
		return &ollamaStreamDetector{}
	case "/api/embed":
		return &objectKeyDetector{key: "embeddings"}
	case "/api/embeddings":
		return &objectKeyDetector{key: "embedding"}
	case "/v1/embeddings":
		return &objectKeyDetector{key: "data"}
	case "/v1/chat/completions", "/v1/completions":
		if strings.HasPrefix(strings.TrimSpace(contentType), "text/event-stream") {
			return &sseDoneDetector{}
		}
		return &completionChoicesDetector{}
	}
	return nil
}

type observedBuffer struct {
	data []byte
	dead bool
}

func (b *observedBuffer) add(chunk []byte) bool {
	if b.dead {
		return false
	}
	if len(b.data)+len(chunk) > maxObservedResponseBytes {
		b.dead = true
		return false
	}
	b.data = append(b.data, chunk...)
	return true
}

// Ollama streams newline-delimited objects and terminates with a "done":true
// object; a non-streaming response is that same object as a single line.
type ollamaStreamDetector struct {
	buf      observedBuffer
	terminal bool
}

func (d *ollamaStreamDetector) observe(chunk []byte, eof bool) bool {
	if d.terminal || !d.buf.add(chunk) {
		return d.terminal
	}
	for {
		line, rest, found := bytes.Cut(d.buf.data, []byte{'\n'})
		if !found {
			break
		}
		d.buf.data = rest
		if d.line(line) {
			return true
		}
	}
	if eof && len(bytes.TrimSpace(d.buf.data)) > 0 {
		d.line(d.buf.data)
	}
	return d.terminal
}

func (d *ollamaStreamDetector) line(line []byte) bool {
	done, valid := ollamaLineDone(line)
	if !valid {
		d.buf.dead = true
		return false
	}
	if done {
		d.terminal = true
	}
	return d.terminal
}

func ollamaLineDone(line []byte) (done, valid bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return false, true
	}
	if strictjson.Check(json.NewDecoder(bytes.NewReader(line))) != nil {
		return false, false
	}
	var object struct {
		Done bool `json:"done"`
	}
	if err := json.Unmarshal(line, &object); err != nil {
		return false, false
	}
	return object.Done, true
}

// OpenAI-compatible servers terminate event streams with a "data: [DONE]"
// event; EOF without it leaves the work unfinished.
type sseDoneDetector struct {
	buf      observedBuffer
	terminal bool
}

func (d *sseDoneDetector) observe(chunk []byte, eof bool) bool {
	if d.terminal || !d.buf.add(chunk) {
		return d.terminal
	}
	for {
		line, rest, found := bytes.Cut(d.buf.data, []byte{'\n'})
		if !found {
			break
		}
		d.buf.data = rest
		if payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok && bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			d.terminal = true
			return true
		}
	}
	return d.terminal
}

// A synchronous response is terminal when the complete body parses as one
// strict JSON object carrying the runtime's result key.
type objectKeyDetector struct {
	buf observedBuffer
	key string
}

func (d *objectKeyDetector) observe(chunk []byte, eof bool) bool {
	if !d.buf.add(chunk) || !eof {
		return false
	}
	if strictjson.Check(json.NewDecoder(bytes.NewReader(d.buf.data))) != nil {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(d.buf.data, &object); err != nil {
		return false
	}
	_, ok := object[d.key]
	return ok
}

// Non-streaming chat/completion responses are terminal when every choice
// carries a non-null finish_reason.
type completionChoicesDetector struct {
	buf observedBuffer
}

func (d *completionChoicesDetector) observe(chunk []byte, eof bool) bool {
	if !d.buf.add(chunk) || !eof {
		return false
	}
	if strictjson.Check(json.NewDecoder(bytes.NewReader(d.buf.data))) != nil {
		return false
	}
	var response struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(d.buf.data, &response); err != nil || len(response.Choices) == 0 {
		return false
	}
	for _, choice := range response.Choices {
		if choice.FinishReason == nil {
			return false
		}
	}
	return true
}
