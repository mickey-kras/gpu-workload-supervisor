package runtime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNativeUnsafeLaunchNotStarted(t *testing.T) {
	for _, launch := range []string{
		"[Service]\nExecStart=/bin/sh -c 'curl https://example.invalid/model -o /tmp/model; vllm serve /tmp/model'\n",
		"[Service]\nEnvironmentFile=/tmp/unverified-native.env\nExecStart=/usr/bin/vllm --model ${MODEL}\n",
		"[Service]\nExecStartPre=/bin/sh -c 'touch /tmp/changed-app-config'\nExecStart=/usr/bin/vllm serve /models/selected\n",
	} {
		m, r, p, _ := nativeFixture(t, "vllm", "http://127.0.0.1:9000")
		if err := os.WriteFile(p.NativeModel.LaunchFile, []byte(launch), 0600); err != nil {
			t.Fatal(err)
		}
		p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(launch)))
		m.config.Catalog.Profiles[2] = p
		err := m.Start(context.Background(), p.ID)
		for _, call := range r.calls {
			if strings.Contains(call, " --user start ") {
				t.Errorf("unsafe launch reached systemctl start: launch=%q err=%v", launch, err)
			}
		}
	}
}

func TestNativeLoadingSingleServerNotHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"Loading model","type":"unavailable_error"}}`)
			return
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"selected","object":"model","meta":null}]}`)
	}))
	defer srv.Close()
	m, _, p, _ := nativeFixture(t, "vllm", srv.URL)
	if err := m.Healthy(context.Background(), p.ID); err == nil {
		t.Error("Healthy accepts model list with meta:null while native /health is 503")
	}
}
