package control

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validCatalog() Catalog {
	return Catalog{Version: 1, Profiles: []Profile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/workloads/speech", HealthURL: "http://127.0.0.1:9000/health"}}}
}

func TestCatalogThirdProfileAndStrictDecode(t *testing.T) {
	c := validCatalog()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Profiles = append(c.Profiles, c.Profiles[0])
	if c.Validate() == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := DecodeCatalog(strings.NewReader(`{"version":1,"profiles":[],"extra":true}`)); err == nil {
		t.Fatal("unknown accepted")
	}
}
func TestStateAcceptsConfiguredIDShape(t *testing.T) {
	s := InitialState("abc", time.Now())
	s.DesiredWorkload = "speech"
	s.ActiveWorkload = "speech"
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestCatalogRejectsDuplicateJSON(t *testing.T) {
	if _, err := DecodeCatalog(strings.NewReader(`{"version":1,"version":1,"profiles":[{"id":"speech","label":"Speech","adapter":"systemd","unit":"speech.service","cgroup":"/user/speech","healthURL":"http://localhost:1"}]}`)); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if _, err := DecodeCatalog(strings.NewReader(`{"version":1,"profiles":[{"id":"speech","id":"vision"}]}`)); err == nil {
		t.Fatal("nested duplicate key accepted")
	}
}
func TestCatalogLabelRejectsInvisibleOrExcessiveText(t *testing.T) {
	for _, label := range []string{"   ", strings.Repeat("a", 81), strings.Repeat("x", 129), "Speech\u202e", "Speech\u2066", "Speech\n", "\xff"} {
		c := validCatalog()
		c.Profiles[0].Label = label
		if c.Validate() == nil {
			t.Fatalf("unsafe label accepted %q", label)
		}
	}
}

func TestWorkloadLabelRejectsByteOrderMark(t *testing.T) {
	for _, label := range []string{"\ufeff", "Speech\ufeff"} {
		if ValidWorkloadLabel(label) {
			t.Fatalf("byte-order-mark label accepted: %q", label)
		}
	}
}

func TestCatalogRejectsInvalidProfiles(t *testing.T) {
	cases := map[string]func(*Catalog){
		"version": func(c *Catalog) { c.Version = 2 }, "empty": func(c *Catalog) { c.Profiles = nil },
		"too many": func(c *Catalog) { c.Profiles = make([]Profile, 33) },
		"reserved": func(c *Catalog) { c.Profiles[0].ID = "idle" }, "uppercase": func(c *Catalog) { c.Profiles[0].ID = "Speech" },
		"empty label": func(c *Catalog) { c.Profiles[0].Label = "" },
		"adapter":     func(c *Catalog) { c.Profiles[0].Adapter = "shell" }, "unit": func(c *Catalog) { c.Profiles[0].Unit = "../speech.service" },
		"root": func(c *Catalog) { c.Profiles[0].Cgroup = "/" }, "relative": func(c *Catalog) { c.Profiles[0].Cgroup = "speech" }, "unclean": func(c *Catalog) { c.Profiles[0].Cgroup = "/workloads/../speech" }, "control group": func(c *Catalog) { c.Profiles[0].Cgroup = "/speech\n" },
		"boot": func(c *Catalog) { c.Profiles[0].BootPolicy = "start" }, "health absent": func(c *Catalog) { c.Profiles[0].HealthURL = "" }, "unload absent": func(c *Catalog) { c.Profiles[0].Adapter = "media-unload" },
		"remote": func(c *Catalog) { c.Profiles[0].HealthURL = "http://192.0.2.1" }, "credentials": func(c *Catalog) { c.Profiles[0].HealthURL = "http://user@localhost" }, "fragment": func(c *Catalog) { c.Profiles[0].HealthURL = "http://localhost/#secret" }, "scheme": func(c *Catalog) { c.Profiles[0].HealthURL = "file:///tmp/x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validCatalog()
			mutate(&c)
			if c.Validate() == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
}

func TestCatalogRejectsOverlappingProfiles(t *testing.T) {
	for _, field := range []string{"unit", "parent", "child"} {
		t.Run(field, func(t *testing.T) {
			c := validCatalog()
			p := c.Profiles[0]
			p.ID = "vision"
			p.Unit = "vision.service"
			p.Cgroup = "/workloads/vision"
			switch field {
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
}

func TestCatalogCloneAndLookup(t *testing.T) {
	c := validCatalog()
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

func TestCatalogDecodeBoundaries(t *testing.T) {
	raw, _ := json.Marshal(validCatalog())
	if _, err := DecodeCatalog(strings.NewReader(string(raw))); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{string(raw) + ` {}`, `{"version":1,"profiles":[`, `{"version":`, `{"version":1,"profiles":[]}`, strings.Repeat(" ", 65537)} {
		if _, err := DecodeCatalog(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if _, err := DecodeCatalog(failedCatalogReader{}); err == nil {
		t.Fatal("ignored reader failure")
	}
}
