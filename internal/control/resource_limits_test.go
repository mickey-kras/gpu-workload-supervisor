package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func fullLimits() ResourceLimits {
	return ResourceLimits{
		Version: 1, MemoryHighMiB: 49152, MemoryMaxMiB: 57344, MemorySwapMaxMiB: 512,
		TasksMax: 4096, CPUWeight: 100, CPUQuotaPercent: 800,
		IODevice: "/dev/nvme0n1", IODeviceNumber: "259:0",
		IOReadBandwidthMax: 524288000, IOWriteBandwidthMax: 262144000,
		IOReadIOPSMax: 20000, IOWriteIOPSMax: 10000, IOWeight: 100,
	}
}

func limitedProfile(l ResourceLimits) WorkloadProfile {
	p := ownedVLLMProfile()
	p.NativeModel.Owned.Limits = l
	return p
}

func TestResourceLimitsValidation(t *testing.T) {
	if err := fullLimits().Validate(); err != nil {
		t.Fatalf("full limits rejected: %v", err)
	}
	for _, ok := range []ResourceLimits{
		{Version: 1, MemoryMaxMiB: MinLimitMemoryMiB},
		{Version: 1, MemoryHighMiB: MaxLimitMemoryMiB},
		{Version: 1, TasksMax: MinLimitTasks},
		{Version: 1, TasksMax: MaxLimitTasks},
		{Version: 1, CPUWeight: 1},
		{Version: 1, CPUQuotaPercent: MaxLimitCPUQuota},
		{Version: 1, IODevice: "/dev/sda", IODeviceNumber: "8:0", IOWriteIOPSMax: MinLimitIOPS},
		{Version: 1, IODevice: "/dev/sda", IODeviceNumber: "8:0", IOReadBandwidthMax: MaxLimitIOBandwidth},
	} {
		if err := ok.Validate(); err != nil {
			t.Fatalf("%+v rejected: %v", ok, err)
		}
	}
	for name, mutate := range map[string]func(*ResourceLimits){
		"version":    func(l *ResourceLimits) { l.Version = 2 },
		"no control": func(l *ResourceLimits) { *l = ResourceLimits{Version: 1} },
		// The memory bound cases clear the other memory value, so only the
		// bounds rule can reject them, not the high-below-max order.
		"memory max low": func(l *ResourceLimits) {
			l.MemoryHighMiB, l.MemoryMaxMiB, l.MemorySwapMaxMiB = 0, MinLimitMemoryMiB-1, 0
		},
		"memory max high": func(l *ResourceLimits) { l.MemoryHighMiB, l.MemoryMaxMiB = 0, MaxLimitMemoryMiB+1 },
		"memory high low": func(l *ResourceLimits) {
			l.MemoryHighMiB, l.MemoryMaxMiB, l.MemorySwapMaxMiB = MinLimitMemoryMiB-1, 0, 0
		},
		"memory high high": func(l *ResourceLimits) {
			l.MemoryHighMiB, l.MemoryMaxMiB, l.MemorySwapMaxMiB = MaxLimitMemoryMiB+1, 0, 0
		},
		"high not below max": func(l *ResourceLimits) { l.MemoryHighMiB = l.MemoryMaxMiB },
		"swap without max":   func(l *ResourceLimits) { l.MemoryMaxMiB, l.MemoryHighMiB = 0, 0 },
		"swap too large":     func(l *ResourceLimits) { l.MemorySwapMaxMiB = MaxLimitMemoryMiB + 1 },
		"tasks low":          func(l *ResourceLimits) { l.TasksMax = MinLimitTasks - 1 },
		"tasks high":         func(l *ResourceLimits) { l.TasksMax = MaxLimitTasks + 1 },
		"cpu weight":         func(l *ResourceLimits) { l.CPUWeight = MaxLimitWeight + 1 },
		"cpu quota":          func(l *ResourceLimits) { l.CPUQuotaPercent = MaxLimitCPUQuota + 1 },
		"io weight":          func(l *ResourceLimits) { l.IOWeight = MaxLimitWeight + 1 },
		"bandwidth low":      func(l *ResourceLimits) { l.IOReadBandwidthMax = MinLimitIOBandwidth - 1 },
		"bandwidth high":     func(l *ResourceLimits) { l.IOWriteBandwidthMax = MaxLimitIOBandwidth + 1 },
		"iops low":           func(l *ResourceLimits) { l.IOReadIOPSMax = MinLimitIOPS - 1 },
		"iops high":          func(l *ResourceLimits) { l.IOWriteIOPSMax = MaxLimitIOPS + 1 },
		"device partition":   func(l *ResourceLimits) { l.IODevice = "/dev/disk/by-id/x" },
		"device spaces":      func(l *ResourceLimits) { l.IODevice = "/dev/sda 1" },
		"device traversal":   func(l *ResourceLimits) { l.IODevice = "/dev/../etc" },
		"device missing":     func(l *ResourceLimits) { l.IODevice = "" },
		"number missing":     func(l *ResourceLimits) { l.IODeviceNumber = "" },
		"number malformed":   func(l *ResourceLimits) { l.IODeviceNumber = "259:0:1" },
		"device without io": func(l *ResourceLimits) {
			*l = ResourceLimits{Version: 1, TasksMax: 64, IODevice: "/dev/sda", IODeviceNumber: "8:0"}
		},
		"number without io":    func(l *ResourceLimits) { *l = ResourceLimits{Version: 1, TasksMax: 64, IODeviceNumber: "8:0"} },
		"device name too long": func(l *ResourceLimits) { l.IODevice = "/dev/a" + strings.Repeat("b", 32) },
	} {
		t.Run(name, func(t *testing.T) {
			l := fullLimits()
			mutate(&l)
			if err := l.Validate(); err == nil {
				t.Fatalf("invalid limits accepted: %+v", l)
			}
		})
	}
}

func TestResourceLimitsRenderLines(t *testing.T) {
	want := "OOMPolicy=stop\nOOMScoreAdjust=500\nMemoryHigh=49152M\nMemoryMax=57344M\nMemorySwapMax=512M\n" +
		"TasksMax=4096\nCPUWeight=100\nCPUQuota=800%\nIOWeight=100\n" +
		"IOReadBandwidthMax=/dev/nvme0n1 524288000\nIOWriteBandwidthMax=/dev/nvme0n1 262144000\n" +
		"IOReadIOPSMax=/dev/nvme0n1 20000\nIOWriteIOPSMax=/dev/nvme0n1 10000\n"
	if got := fullLimits().RenderLines(); got != want {
		t.Fatalf("render drift:\n%s", got)
	}
	// MemoryMax without a swap value renders the automatic MemorySwapMax=0.
	if got := (ResourceLimits{Version: 1, MemoryMaxMiB: 1024}).RenderLines(); got != "OOMPolicy=stop\nOOMScoreAdjust=500\nMemoryMax=1024M\nMemorySwapMax=0M\n" {
		t.Fatalf("swap default drift:\n%s", got)
	}
	if got := (ResourceLimits{Version: 1, TasksMax: 64}).RenderLines(); got != "OOMPolicy=stop\nOOMScoreAdjust=500\nTasksMax=64\n" {
		t.Fatalf("tasks-only render drift:\n%s", got)
	}
	if (ResourceLimits{}).RenderLines() != "" {
		t.Fatal("absent limits rendered lines")
	}
}

func TestLimitsRequireCatalogVersion5(t *testing.T) {
	p := limitedProfile(fullLimits())
	for _, version := range []int{2, 4} {
		if err := (Catalog{Version: version, Profiles: []WorkloadProfile{p}}).Validate(); !errors.Is(err, ErrLimitsRequireV5) {
			t.Fatalf("limits in v%d = %v", version, err)
		}
	}
	if err := (Catalog{Version: 5, Profiles: []WorkloadProfile{p}}).Validate(); err != nil {
		t.Fatalf("v5 limits rejected: %v", err)
	}
	// Version 5 accepts version 2 and version 4 content without limits.
	if err := (Catalog{Version: 5, Profiles: []WorkloadProfile{ownedVLLMProfile()}}).Validate(); err != nil {
		t.Fatalf("v5 owned without limits rejected: %v", err)
	}
	trainer := contractTrainerCatalog().Profiles[0]
	for _, tc := range []struct {
		profiles []WorkloadProfile
		lowest   int
	}{{[]WorkloadProfile{trainer}, 4}, {[]WorkloadProfile{trainer, p}, 5}} {
		c := Catalog{Version: 5, Profiles: tc.profiles}
		if err := c.Validate(); err != nil {
			t.Fatalf("v5 with a trainer rejected: %v", err)
		}
		if got := LowestCatalogVersion(c); got != tc.lowest {
			t.Fatalf("lowest version = %d, want %d", got, tc.lowest)
		}
	}
	if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{trainer}}).Validate(); err == nil {
		t.Fatal("v2 accepted a trainer")
	}
	invalid := limitedProfile(ResourceLimits{Version: 1, TasksMax: 1})
	if err := (Catalog{Version: 5, Profiles: []WorkloadProfile{invalid}}).Validate(); err == nil {
		t.Fatal("v5 accepted an out-of-range limit")
	}
	if err := (Catalog{Version: 6, Profiles: []WorkloadProfile{p}}).Validate(); err == nil {
		t.Fatal("unknown catalog version accepted")
	}
}

func TestLimitsStrictDecode(t *testing.T) {
	c := Catalog{Version: 5, Profiles: []WorkloadProfile{limitedProfile(fullLimits())}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCatalogBytes(raw)
	if err != nil || decoded.Profiles[0].NativeModel.Owned.Limits != fullLimits() {
		t.Fatalf("v5 round trip: %v", err)
	}
	for name, edit := range map[string]func(string) string{
		"unknown limit field": func(s string) string {
			return strings.Replace(s, `"tasksMax":4096`, `"tasksMax":4096,"cpuSet":"0-3"`, 1)
		},
		"duplicate limit key": func(s string) string {
			return strings.Replace(s, `"tasksMax":4096`, `"tasksMax":4096,"tasksMax":64`, 1)
		},
		"limits in v4": func(s string) string { return strings.Replace(s, `"version":5`, `"version":4`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCatalogBytes([]byte(edit(string(raw)))); err == nil {
				t.Fatal("tampered limits accepted")
			}
		})
	}
	legacy, err := json.Marshal(Catalog{Version: 2, Profiles: []WorkloadProfile{ownedVLLMProfile()}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacy, []byte("limits")) {
		t.Fatal("absent limits changed the legacy encoding")
	}
	for _, empty := range []string{`{}`, `{"version":0}`, `null`, ` null `} {
		smuggled := strings.Replace(string(legacy), `"port":9100`, `"port":9100,"limits":`+empty, 1)
		smuggled = strings.Replace(smuggled, `"version":2`, `"version":5`, 1)
		if _, err := DecodeCatalogBytes([]byte(smuggled)); !errors.Is(err, ErrEmptyLimits) {
			t.Fatalf("empty limits %s = %v", empty, err)
		}
		// Every decoder of the type applies the rule, not only the catalog path.
		var lax Catalog
		if err := json.Unmarshal([]byte(smuggled), &lax); !errors.Is(err, ErrEmptyLimits) {
			t.Fatalf("lax decode of empty limits %s = %v", empty, err)
		}
	}
	var unknown ResourceLimits
	if err := json.Unmarshal([]byte(`{"version":1,"tasksMax":64,"cpuSet":"0"}`), &unknown); err == nil {
		t.Fatal("lax decode accepted an unknown limit field")
	}
}

func TestLowestCatalogVersionRoundTrip(t *testing.T) {
	trainer := WorkloadProfile{Trainer: &TrainerBinding{Version: 1}}
	plain := validCatalog().Profiles[0]
	for _, tc := range []struct {
		profiles []WorkloadProfile
		want     int
	}{
		{nil, 1},
		{[]WorkloadProfile{plain}, 1},
		{[]WorkloadProfile{plain, ownedVLLMProfile()}, 2},
		{[]WorkloadProfile{ownedVLLMProfile(), trainer}, 4},
		{[]WorkloadProfile{trainer, limitedProfile(fullLimits())}, 5},
	} {
		if got := LowestCatalogVersion(Catalog{Profiles: tc.profiles}); got != tc.want {
			t.Fatalf("lowest version = %d, want %d", got, tc.want)
		}
		if _, limited := (Catalog{Profiles: tc.profiles}).LimitedProfile(); limited != (tc.want == CatalogVersionLimits) {
			t.Fatalf("limited profile lookup for version %d = %v", tc.want, limited)
		}
	}
	// Adding limits writes v5; removing them writes the exact earlier bytes.
	before := Catalog{Version: 2, Profiles: []WorkloadProfile{ownedVLLMProfile()}}
	original, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	limited := before.Clone()
	limited.Profiles[0].NativeModel.Owned.Limits = fullLimits()
	limited.Version = LowestCatalogVersion(limited)
	if limited.Version != 5 || !limited.HasLimits() || before.HasLimits() {
		t.Fatalf("limited catalog version %d", limited.Version)
	}
	restored := limited.Clone()
	restored.Profiles[0].NativeModel.Owned.Limits = ResourceLimits{}
	restored.Version = LowestCatalogVersion(restored)
	again, err := json.Marshal(restored)
	if err != nil || !bytes.Equal(original, again) {
		t.Fatalf("rollback bytes differ:\n%s\n%s", original, again)
	}
}

func TestLimitsCompareAndCloneByValue(t *testing.T) {
	a := limitedProfile(fullLimits())
	b := limitedProfile(fullLimits())
	// == compiles only while OwnedLaunch and ResourceLimits stay comparable.
	if *a.NativeModel.Owned != *b.NativeModel.Owned {
		t.Fatal("equal limits compare unequal")
	}
	b.NativeModel.Owned.Limits.TasksMax = 64
	if *a.NativeModel.Owned == *b.NativeModel.Owned {
		t.Fatal("different limits compare equal")
	}
	c := Catalog{Version: 5, Profiles: []WorkloadProfile{a}}
	clone := c.Clone()
	clone.Profiles[0].NativeModel.Owned.Limits.TasksMax = 64
	if c.Profiles[0].NativeModel.Owned.Limits.TasksMax != 4096 {
		t.Fatal("clone shares limits")
	}
}
