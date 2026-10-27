package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStoredLegacyCatalogRecoveryRemainsStrict(t *testing.T) {
	c := validCatalog()
	c.Profiles[0].NativeModel = &NativeModel{Runtime: "ollama", Instance: "old", Model: "model", Endpoint: "http://127.0.0.1:9000", LaunchFile: "/home/test/old.service", LaunchSHA256: strings.Repeat("a", 64)}
	raw, _ := json.Marshal(c)
	recovered, err := DecodeStoredCatalog(raw)
	if err != nil || !recovered.Profiles[0].RemovedRuntime() {
		t.Fatal(recovered, err)
	}
	if recovered.Validate() == nil {
		t.Fatal("legacy execution accepted")
	}
	for _, bad := range []string{
		strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(raw), `"version":1`, `"extra":true,"version":1`, 1),
		strings.Replace(string(raw), `"runtime":"ollama"`, `"runtime":"ollama","extra":true`, 1),
		strings.Replace(string(raw), `"label":"Speech"`, `"label":"Speech","extra":true`, 1),
		strings.Replace(string(raw), `"runtime":"ollama"`, `"runtime":"unknown"`, 1),
		string(raw) + " {}",
	} {
		if _, err := DecodeStoredCatalog([]byte(bad)); err == nil {
			t.Fatalf("invalid stored catalog accepted: %s", bad)
		}
	}
	sibling := c.Profiles[0]
	sibling.ID = "second"
	native := *sibling.NativeModel
	native.Model = "other"
	sibling.NativeModel = &native
	c.Profiles = append(c.Profiles, sibling)
	raw, _ = json.Marshal(c)
	if recovered, err := DecodeStoredCatalog(raw); err != nil || len(recovered.Profiles) != 2 {
		t.Fatal("shared unit not recoverable", err)
	}
	c.Profiles[1].NativeModel.LaunchSHA256 = strings.Repeat("b", 64)
	raw, _ = json.Marshal(c)
	if _, err := DecodeStoredCatalog(raw); err == nil {
		t.Fatal("conflicting shared launch accepted")
	}
}

func TestStoredLegacyCatalogRejectsUnsafeRemovalEvidence(t *testing.T) {
	base := validCatalog()
	base.Profiles[0].NativeModel = &NativeModel{Runtime: "llama.cpp", Instance: "old", Model: "model", Endpoint: "http://127.0.0.1:9000", LaunchFile: "/home/test/old.service", LaunchSHA256: strings.Repeat("a", 64)}
	for name, mutate := range map[string]func(*Catalog){
		"invalid identity": func(c *Catalog) { c.Profiles[0].NativeModel.Instance = "../old" },
		"invalid endpoint": func(c *Catalog) { c.Profiles[0].NativeModel.Endpoint = "http://example.com" },
		"invalid dropin": func(c *Catalog) {
			c.Profiles[0].NativeModel.DropIns = []LaunchSource{{Path: "/tmp/override.conf", SHA256: "invalid"}}
		},
		"media adapter":   func(c *Catalog) { c.Profiles[0].Adapter = AdapterMediaUnload },
		"owned placement": func(c *Catalog) { c.Version = 2; c.Profiles[0].NativeModel.Owned = &OwnedLaunch{Port: 9000} },
		"too many profiles": func(c *Catalog) {
			for len(c.Profiles) < 33 {
				c.Profiles = append(c.Profiles, c.Profiles[0])
			}
		},
		"duplicate identity":     func(c *Catalog) { c.Profiles = append(c.Profiles, c.Profiles[0]) },
		"disabled with profiles": func(c *Catalog) { c.Disabled = true },
	} {
		t.Run(name, func(t *testing.T) {
			c := base.Clone()
			mutate(&c)
			raw, _ := json.Marshal(c)
			if _, err := DecodeStoredCatalog(raw); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
		})
	}
	if _, err := DecodeStoredCatalog(make([]byte, MaxCatalogBytes+1)); err == nil {
		t.Fatal("oversized recovery accepted")
	}
	for _, raw := range []string{`{"version":1,"profiles":[{"nativeModel":4}]}`, `{"version":1,"profiles":[{"nativeModel":{"runtime":"llama.cpp","owned":{"unexpected":true}}}]}`} {
		if _, err := DecodeStoredCatalog([]byte(raw)); err == nil {
			t.Fatal("invalid schema accepted")
		}
	}
}
