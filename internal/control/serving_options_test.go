package control

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func servingProfile(s ServingOptions) WorkloadProfile {
	p := ownedVLLMProfile()
	p.NativeModel.Owned.Serving = s
	return p
}

func TestServingOptionsRenderDefaultsExplicitly(t *testing.T) {
	got := ServingOptions{Version: ServingOptionsVersion}.Args()
	want := []string{
		"--gpu-memory-utilization", "0.90",
		"--kv-cache-dtype", "auto",
		"--enable-chunked-prefill",
		"--enable-prefix-caching", "--prefix-caching-hash-algo", "sha256",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("defaults = %q, want %q", got, want)
	}
	if args := (ServingOptions{}).Args(); args != nil {
		t.Fatalf("absent options rendered %q", args)
	}
	full := ServingOptions{Version: 1, GPUMemoryPercent: 75, KVCacheDType: "auto", MaxNumSeqs: 8, MaxNumBatchedTokens: 4096, DisablePrefixCaching: true}
	want = []string{
		"--gpu-memory-utilization", "0.75",
		"--kv-cache-dtype", "auto",
		"--enable-chunked-prefill",
		"--no-enable-prefix-caching",
		"--max-num-seqs", "8",
		"--max-num-batched-tokens", "4096",
	}
	if got := full.Args(); !slices.Equal(got, want) {
		t.Fatalf("full = %q, want %q", got, want)
	}
	sleeping := ServingOptions{Version: 1, SleepMode: true}.Args()
	if !slices.Contains(sleeping, FlagEnableSleepMode) {
		t.Fatalf("sleepMode did not render %s: %q", FlagEnableSleepMode, sleeping)
	}
}

func TestServingOptionsBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    ServingOptions
		ok   bool
	}{
		{"defaults", ServingOptions{Version: 1}, true},
		{"version", ServingOptions{Version: 2}, false},
		{"utilization below", ServingOptions{Version: 1, GPUMemoryPercent: MinGPUMemoryPercent - 1}, false},
		{"utilization at minimum", ServingOptions{Version: 1, GPUMemoryPercent: MinGPUMemoryPercent}, true},
		{"utilization at maximum", ServingOptions{Version: 1, GPUMemoryPercent: MaxGPUMemoryPercent}, true},
		{"utilization above", ServingOptions{Version: 1, GPUMemoryPercent: MaxGPUMemoryPercent + 1}, false},
		{"sequences at minimum", ServingOptions{Version: 1, MaxNumSeqs: 1}, true},
		{"sequences at maximum", ServingOptions{Version: 1, MaxNumSeqs: MaxServingNumSeqs}, true},
		{"sequences above", ServingOptions{Version: 1, MaxNumSeqs: MaxServingNumSeqs + 1}, false},
		{"batched below", ServingOptions{Version: 1, MaxNumBatchedTokens: MinBatchedTokens - 1}, false},
		{"batched at minimum", ServingOptions{Version: 1, MaxNumBatchedTokens: MinBatchedTokens}, true},
		{"batched at maximum", ServingOptions{Version: 1, MaxNumBatchedTokens: MaxBatchedTokens}, true},
		{"batched above", ServingOptions{Version: 1, MaxNumBatchedTokens: MaxBatchedTokens + 1}, false},
		{"dtype auto", ServingOptions{Version: 1, KVCacheDType: "auto"}, true},
		{"dtype fp8 unqualified", ServingOptions{Version: 1, KVCacheDType: "fp8"}, false},
		{"dtype other", ServingOptions{Version: 1, KVCacheDType: "bfloat16"}, false},
	} {
		err := tc.s.Validate()
		if (err == nil) != tc.ok {
			t.Fatalf("%s: Validate = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
	if err := ValidKVCacheDType("fp8"); err == nil || !strings.Contains(err.Error(), "#453") {
		t.Fatalf("fp8 refusal = %v, want qualification reason", err)
	}
}

func TestServingOptionsRequireCatalogVersion6(t *testing.T) {
	p := servingProfile(ServingOptions{Version: 1})
	for _, version := range []int{2, 4, 5} {
		if err := (Catalog{Version: version, Profiles: []WorkloadProfile{p}}).Validate(); !errors.Is(err, ErrServingRequiresV6) {
			t.Fatalf("version %d = %v, want ErrServingRequiresV6", version, err)
		}
	}
	c := Catalog{Version: 6, Profiles: []WorkloadProfile{p}}
	if err := c.Validate(); err != nil {
		t.Fatalf("v6 serving rejected: %v", err)
	}
	if got := LowestCatalogVersion(c); got != CatalogVersionServing {
		t.Fatalf("lowest version = %d, want 6", got)
	}
	limited := limitedProfile(ResourceLimits{Version: 1, MemoryMaxMiB: 4096})
	limited.ID, limited.Unit, limited.Cgroup = "other", "gws-owned-other.service", "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-other.service"
	if got := LowestCatalogVersion(Catalog{Profiles: []WorkloadProfile{limited, p}}); got != CatalogVersionServing {
		t.Fatalf("limits with serving lowest version = %d, want 6", got)
	}
	if got := LowestCatalogVersion(Catalog{Profiles: []WorkloadProfile{ownedVLLMProfile()}}); got != CatalogVersionOwned {
		t.Fatalf("owned without serving lowest version = %d, want 2", got)
	}
	invalid := servingProfile(ServingOptions{Version: 1, MaxNumSeqs: 65})
	if err := (Catalog{Version: 6, Profiles: []WorkloadProfile{invalid}}).Validate(); err == nil {
		t.Fatal("v6 accepted an out-of-range serving option")
	}
}

func TestServingOptionsStrictDecode(t *testing.T) {
	c := Catalog{Version: 6, Profiles: []WorkloadProfile{servingProfile(ServingOptions{Version: 1, GPUMemoryPercent: 80, MaxNumSeqs: 4})}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCatalogBytes(raw)
	if err != nil || decoded.Profiles[0].NativeModel.Owned.Serving != c.Profiles[0].NativeModel.Owned.Serving {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
	for _, bad := range []string{`{}`, `null`, `{"version":1,"unknown":1}`, `{"version":1,"version":1}`} {
		text := strings.Replace(string(raw), `"serving":{"version":1,"gpuMemoryPercent":80,"maxNumSeqs":4}`, `"serving":`+bad, 1)
		if text == string(raw) {
			t.Fatal("fixture did not change")
		}
		if _, err := DecodeCatalogBytes([]byte(text)); err == nil {
			t.Fatalf("serving %s accepted", bad)
		}
	}
	// Absent options keep the exact bytes without a serving key.
	plain, err := json.Marshal(ownedVLLMProfile())
	if err != nil || strings.Contains(string(plain), "serving") {
		t.Fatalf("absent serving options encoded: %s, %v", plain, err)
	}
}

func TestGPUReservation(t *testing.T) {
	for _, tc := range []struct {
		percent uint32
		total   uint64
		want    uint64
	}{{90, 24576, 22119}, {50, 24576, 12288}, {95, 81920, 77824}, {90, 1, 1}, {90, ^uint64(0), ^uint64(0)}} {
		if got := GPUReservationMiB(tc.percent, tc.total); got != tc.want {
			t.Fatalf("GPUReservationMiB(%d, %d) = %d, want %d", tc.percent, tc.total, got, tc.want)
		}
	}
	if FormatGPUMemoryUtilization(50) != "0.50" || FormatGPUMemoryUtilization(95) != "0.95" {
		t.Fatal("utilization format")
	}
}

func TestSleepingOwnedUnitsDoNotStopTheirPeersThroughConflicts(t *testing.T) {
	p := servingProfile(ServingOptions{Version: 1, SleepMode: true})
	q := ownedVLLMProfile()
	q.ID, q.Unit = "other", "gws-owned-other.service"
	c := Catalog{Profiles: []WorkloadProfile{p, q}}
	if c.OwnedConflictUnits(p) != "" || c.OwnedConflictUnits(q) != "" {
		t.Fatal("systemd would stop the sleeping process and lose its RAM cache")
	}
	p.NativeModel.Owned.Serving.SleepMode = false
	if c.OwnedConflictUnits(p) != q.Unit || c.OwnedConflictUnits(q) != p.Unit {
		t.Fatal("ordinary owned units lost their conflict backstop")
	}
}
