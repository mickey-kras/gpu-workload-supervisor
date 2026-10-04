package control

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func acceptanceCatalog() Catalog {
	return Catalog{Version: 1, Profiles: []Profile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/workloads/speech", HealthURL: "http://127.0.0.1:9000/health"}}}
}
func TestCatalogValidationAcceptance(t *testing.T) {
	cases := map[string]func(*Catalog){
		"version": func(c *Catalog) { c.Version = 2 }, "empty": func(c *Catalog) { c.Profiles = nil },
		"too many": func(c *Catalog) { c.Profiles = make([]Profile, 33) },
		"reserved": func(c *Catalog) { c.Profiles[0].ID = "idle" }, "uppercase": func(c *Catalog) { c.Profiles[0].ID = "Speech" },
		"empty label": func(c *Catalog) { c.Profiles[0].Label = "" }, "control label": func(c *Catalog) { c.Profiles[0].Label = "Speech\n" }, "utf8 label": func(c *Catalog) { c.Profiles[0].Label = "\xff" }, "long label": func(c *Catalog) { c.Profiles[0].Label = strings.Repeat("x", 129) },
		"adapter": func(c *Catalog) { c.Profiles[0].Adapter = "shell" }, "unit": func(c *Catalog) { c.Profiles[0].Unit = "../speech.service" },
		"root": func(c *Catalog) { c.Profiles[0].Cgroup = "/" }, "relative": func(c *Catalog) { c.Profiles[0].Cgroup = "speech" }, "unclean": func(c *Catalog) { c.Profiles[0].Cgroup = "/workloads/../speech" }, "control group": func(c *Catalog) { c.Profiles[0].Cgroup = "/speech\n" },
		"boot": func(c *Catalog) { c.Profiles[0].BootPolicy = "start" }, "health absent": func(c *Catalog) { c.Profiles[0].HealthURL = "" }, "unload absent": func(c *Catalog) { c.Profiles[0].Adapter = "media-unload" },
		"remote": func(c *Catalog) { c.Profiles[0].HealthURL = "http://192.0.2.1" }, "credentials": func(c *Catalog) { c.Profiles[0].HealthURL = "http://user@localhost" }, "fragment": func(c *Catalog) { c.Profiles[0].HealthURL = "http://localhost/#secret" }, "scheme": func(c *Catalog) { c.Profiles[0].HealthURL = "file:///tmp/x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := acceptanceCatalog()
			mutate(&c)
			if c.Validate() == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
	for _, field := range []string{"id", "unit", "parent", "child"} {
		t.Run("overlap "+field, func(t *testing.T) {
			c := acceptanceCatalog()
			p := c.Profiles[0]
			p.ID = "vision"
			p.Unit = "vision.service"
			p.Cgroup = "/workloads/vision"
			switch field {
			case "id":
				p.ID = "speech"
			case "unit":
				p.Unit = "speech.service"
			case "parent":
				p.Cgroup = "/workloads"
			case "child":
				p.Cgroup = "/workloads/speech/child"
			}
			c.Profiles = append(c.Profiles, p)
			if c.Validate() == nil {
				t.Fatal("overlap accepted")
			}
		})
	}
	c := acceptanceCatalog()
	clone := c.Clone()
	clone.Profiles[0].Label = "Other"
	if c.Profiles[0].Label != "Speech" {
		t.Fatal("clone aliases")
	}
	if _, ok := c.Profile("unconfigured"); ok {
		t.Fatal("unknown profile found")
	}
	for _, url := range []string{"http://localhost:90/health", "https://[::1]/health"} {
		c.Profiles[0].HealthURL = url
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

type failedCatalogReader struct{}

func (failedCatalogReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func TestCatalogDecodeAcceptance(t *testing.T) {
	raw, _ := json.Marshal(acceptanceCatalog())
	if _, err := DecodeCatalog(strings.NewReader(string(raw))); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{string(raw) + ` {}`, `{"version":1,"profiles":[{"id":"speech","id":"vision"}]}`, `{"version":1,"profiles":[`, `{"version":`, `{"version":1,"profiles":[]}`, strings.Repeat(" ", 65537)} {
		if _, err := DecodeCatalog(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if _, err := DecodeCatalog(failedCatalogReader{}); err == nil {
		t.Fatal("ignored reader failure")
	}
}
