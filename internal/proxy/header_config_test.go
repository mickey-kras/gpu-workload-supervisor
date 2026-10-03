package proxy

import (
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"net/url"
	"testing"
)

func TestConfiguredHeaderNames(t *testing.T) {
	upstream, _ := url.Parse("http://localhost:8080")
	for _, field := range []string{"request", "job", "incarnation", "epoch"} {
		t.Run(field, func(t *testing.T) {
			for _, name := range []string{"X Bad", "X:Bad", "X\tBad", "X\r\nBad", "X-☃", "X-\x7f", "X-Valid_!#$%&'*+-.^`|~"} {
				config := Config{Upstream: upstream, Workload: control.WorkloadText, ExecutionRoutes: []Route{{Method: "POST", Path: "/execute"}}}
				switch field {
				case "request":
					config.RequestIDHeader = name
				case "job":
					config.JobIDHeader = name
				case "incarnation":
					config.FenceIDHeader = name
				case "epoch":
					config.FenceEpochHeader = name
				}
				err := ValidateConfig(config)
				valid := name == "X-Valid_!#$%&'*+-.^`|~"
				if (err == nil) != valid {
					t.Errorf("header %q: error %v, valid=%v", name, err, valid)
				}
			}
		})
	}
}
