package control

import (
	"strings"
	"testing"
	"time"
)

func TestCatalogThirdProfileAndStrictDecode(t *testing.T) {
	c := Catalog{Version: 1, Profiles: []Profile{{ID: "speech", Label: "Speech", Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://127.0.0.1:9000/health"}}}
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
}
func TestCatalogLabelRejectsInvisibleOrExcessiveText(t *testing.T) {
	for _, label := range []string{"   ", strings.Repeat("a", 81), "Speech\u202e", "Speech\u2066"} {
		c := Catalog{Version: 1, Profiles: []Profile{{ID: "speech", Label: label, Adapter: "systemd", Unit: "speech.service", Cgroup: "/user/speech", HealthURL: "http://localhost:1"}}}
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
