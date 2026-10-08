package control

import "testing"

func TestDisabledCatalogIsExplicitAndCannotHideProfiles(t *testing.T) {
	for _, version := range []int{1, 2} {
		if _, err := DecodeCatalogBytes([]byte(`{"version":` + string(rune('0'+version)) + `,"profiles":[],"disabled":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`{"version":1,"profiles":[]}`, `{"version":1,"profiles":[],"disabled":false}`, `{"version":3,"profiles":[],"disabled":true}`, `{"version":1,"profiles":[],"disabled":true,"disabled":false}`} {
		if _, err := DecodeCatalogBytes([]byte(raw)); err == nil {
			t.Fatalf("unsafe empty catalog accepted: %s", raw)
		}
	}
	c := validCatalog()
	c.Disabled = true
	if c.Validate() == nil {
		t.Fatal("disabled catalog hid a configured workload")
	}
}

func TestDisabledCatalogClonePreservesExplicitEmptyProfiles(t *testing.T) {
	for _, profiles := range [][]WorkloadProfile{nil, {}} {
		catalog := Catalog{Version: 1, Disabled: true, Profiles: profiles}
		if (catalog.Clone().Profiles == nil) != (profiles == nil) {
			t.Fatal("clone changed empty catalog representation")
		}
	}
}
