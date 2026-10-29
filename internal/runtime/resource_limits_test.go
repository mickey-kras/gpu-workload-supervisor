package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func limitedOwnedProfile(t *testing.T) (control.WorkloadProfile, []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vllm"), []byte("#!/bin/false\n"), 0700); err != nil {
		t.Fatal(err)
	}
	saved := ownedBinaryDirectory
	ownedBinaryDirectory = dir
	t.Cleanup(func() { ownedBinaryDirectory = saved })
	modelDir := filepath.Join(dir, "v")
	if err := os.Mkdir(modelDir, 0700); err != nil {
		t.Fatal(err)
	}
	limits := control.ResourceLimits{Version: 1, MemoryHighMiB: 4096, MemoryMaxMiB: 8192, TasksMax: 4096, CPUQuotaPercent: 400,
		IODevice: "/dev/nvme0n1", IODeviceNumber: "259:0", IOReadBandwidthMax: 1 << 30}
	p := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: modelDir, Port: 9100, Alias: "selected", Conflicts: "gws-owned-alpha.service", Limits: limits})
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	return p, raw
}

func TestRenderOwnedUnitWithLimitsGolden(t *testing.T) {
	p, raw := limitedOwnedProfile(t)
	want := "[Unit]\nDescription=Supervisor-owned vllm launch for vision\nConflicts=gws-owned-alpha.service\nAfter=gws-owned-alpha.service\n" +
		"\n[Service]\nType=simple\nRestart=no\n" +
		"OOMPolicy=stop\nOOMScoreAdjust=500\nMemoryHigh=4096M\nMemoryMax=8192M\nMemorySwapMax=0M\nTasksMax=4096\nCPUQuota=400%\n" +
		"IOReadBandwidthMax=/dev/nvme0n1 1073741824\n" +
		"ExecStart=" + ownedBinaryDirectory + "/vllm serve " + p.NativeModel.Owned.ModelPath + " --host 127.0.0.1 --port 9100 --served-model-name selected\n"
	if string(raw) != want {
		t.Fatalf("render drift:\n%s", raw)
	}
	// Removing the limits restores the exact legacy bytes and fingerprint.
	legacy := p
	n := *p.NativeModel
	o := *n.Owned
	o.Limits = control.ResourceLimits{}
	n.Owned = &o
	legacy.NativeModel = &n
	plain, err := RenderOwnedUnit(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "OOMPolicy") || string(plain) != strings.Replace(want, p.NativeModel.Owned.Limits.RenderLines(), "", 1) {
		t.Fatalf("legacy render drift:\n%s", plain)
	}
}

func TestOwnedLimitsParseOnlyAtRenderedPosition(t *testing.T) {
	p, raw := limitedOwnedProfile(t)
	n := *p.NativeModel
	if err := qualifyFixtureLaunch(raw, n); err != nil {
		t.Fatalf("rendered limits do not qualify: %v", err)
	}
	block := n.Owned.Limits.RenderLines()
	exec := raw[strings.Index(string(raw), "ExecStart="):]
	for name, edited := range map[string]string{
		"moved after ExecStart":  strings.Replace(string(raw), block, "", 1) + block,
		"changed value":          strings.Replace(string(raw), "TasksMax=4096", "TasksMax=8192", 1),
		"directive before block": strings.Replace(string(raw), "Restart=no\n", "Restart=no\nTasksMax=64\n", 1),
		"duplicated block":       strings.Replace(string(raw), block, block+block, 1),
		"missing block":          strings.Replace(string(raw), block, "", 1),
		"extra limit line":       strings.Replace(string(raw), string(exec), "CPUWeight=50\n"+string(exec), 1),
		"second service header":  string(raw) + "\n[Service]\nType=simple\nRestart=no\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := qualifyFixtureLaunch([]byte(edited), n); err == nil {
				t.Fatal("edited limits qualified")
			}
		})
	}
	if _, err := removeOwnedLimitLines([]byte("[Service]\nExecStart=/x\n"), n.Owned.Limits); !errors.Is(err, ErrLaunchUnsupported) {
		t.Fatalf("missing header = %v", err)
	}
}

func TestAdoptedGrammarRejectsLimitDirectives(t *testing.T) {
	n := control.NativeModel{Runtime: "vllm", Endpoint: "http://127.0.0.1:9100", Model: "/models/v"}
	for _, line := range []string{"OOMPolicy=stop", "OOMScoreAdjust=500", "MemoryHigh=1G", "MemoryMax=2G", "MemorySwapMax=0",
		"TasksMax=64", "CPUWeight=50", "CPUQuota=100%", "IOWeight=100", "IOReadBandwidthMax=/dev/sda 1M",
		"IOWriteBandwidthMax=/dev/sda 1M", "IOReadIOPSMax=/dev/sda 100", "IOWriteIOPSMax=/dev/sda 100"} {
		raw := "[Service]\nType=simple\n" + line + "\nExecStart=/usr/bin/vllm serve /models/v --host 127.0.0.1 --port 9100\n"
		if err := qualifyFixtureLaunch([]byte(raw), n); !errors.Is(err, ErrLaunchUnsupported) {
			t.Fatalf("adopted %s accepted: %v", line, err)
		}
	}
	if resourceLimitDirective("Description") {
		t.Fatal("unrelated directive classified as a limit")
	}
}

func TestUnitWithLimitsDoesNotLaunchUntilEnforced(t *testing.T) {
	p, _ := limitedOwnedProfile(t)
	if err := requireEnforceableLimits(p); !errors.Is(err, ErrLimitsNotEnforced) {
		t.Fatalf("limits accepted for launch: %v", err)
	}
	m := &SystemdManager{}
	if err := m.verifyNativeBinding(context.Background(), p); !errors.Is(err, ErrLimitsNotEnforced) {
		t.Fatalf("binding verification accepted limits: %v", err)
	}
	plain := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: "/models/v", Port: 9100})
	if err := requireEnforceableLimits(plain); err != nil {
		t.Fatalf("unit without limits blocked: %v", err)
	}
	if err := requireEnforceableLimits(control.WorkloadProfile{}); err != nil {
		t.Fatalf("profile without a native model blocked: %v", err)
	}
}
