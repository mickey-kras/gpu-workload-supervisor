package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestRequesterComfyUIDurableDispatch(t *testing.T) {
	ctx := context.Background()
	f := newContractFixture(t)
	body := `{"prompt":{"1":{"class_type":"FixtureOutput","inputs":{}}}}`
	posts := 0
	runtimeURL := ""
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		attempts, err := f.store.PendingComfyUIAttempts(ctx, control.WorkloadMedia, runtimeURL, ComfyUIContract, "", 64)
		if err != nil || len(attempts) != 1 {
			t.Error("dispatch before durable attempt", err)
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(payload), "FixtureOutput") {
			t.Error("authorized body was lost", err)
		}
		if r.Header.Get(RequesterCredentialHeader) != "" || r.Header.Get(WorkflowHeader) != "" {
			t.Error("requester metadata leaked")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer runtime.Close()
	runtimeURL = runtime.URL
	target, _ := url.Parse(runtime.URL)
	c := requesterConfig()
	c.Upstream = target
	c.ComfyUIContract = ComfyUIContract
	c.ExecutionRoutes = []Route{{"POST", "/prompt"}}
	c.RequesterPolicy.Requesters[0].Workflows[0].Path = "/prompt"
	c.RequesterPolicy.Requesters[0].Workflows[0].BodySHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	h, err := NewWithContext(ctx, f.store, c)
	if err != nil {
		t.Fatal(err)
	}
	send := func() int {
		r := requesterRequest()
		r.URL.Path = "/prompt"
		r.Body = io.NopCloser(strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := send(); got != 202 {
		t.Fatal(got)
	}
	if pending, err := f.store.PendingWork(ctx); err != nil || pending != 1 {
		t.Fatalf("pending %d %v", pending, err)
	}
	if got := send(); got != 409 || posts != 1 {
		t.Fatalf("replayed ComfyUI: %d posts %d", got, posts)
	}
}
