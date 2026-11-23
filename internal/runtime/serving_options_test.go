package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

const gpuFreeTotalCommand = "/usr/bin/true --query-gpu=memory.free,memory.total --format=csv,noheader,nounits -i 0"

func servingRenderProfile(serving control.ServingOptions) control.WorkloadProfile {
	return ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: "/models/v", Port: 9100, Serving: serving})
}

func TestRenderOwnedUnitServingDefaultsGolden(t *testing.T) {
	raw, err := RenderOwnedUnit(servingRenderProfile(control.ServingOptions{Version: 1}))
	if err != nil {
		t.Fatal(err)
	}
	want := "[Unit]\nDescription=Supervisor-owned vllm launch for vision\n\n[Service]\nType=simple\nRestart=no\n" +
		"Environment=HF_HUB_OFFLINE=1\nEnvironmentFile=/home/u/.config/gpu-workload-supervisor/vllm/vision.env\n" +
		"ExecStart=/usr/bin/vllm serve /models/v --host 127.0.0.1 --port 9100 --load-format safetensors --chat-template /models/v/chat_template.jinja" +
		" --gpu-memory-utilization 0.90 --kv-cache-dtype auto --enable-chunked-prefill --enable-prefix-caching --prefix-caching-hash-algo sha256\n"
	if string(raw) != want {
		t.Fatalf("serving defaults render drift:\n%s", raw)
	}
	// Absent options keep the exact earlier bytes.
	plain, err := RenderOwnedUnit(servingRenderProfile(control.ServingOptions{}))
	if err != nil || strings.Contains(string(plain), "--gpu-memory-utilization") {
		t.Fatalf("absent options rendered: %s, %v", plain, err)
	}
	if _, err := RenderOwnedUnit(servingRenderProfile(control.ServingOptions{Version: 1, MaxNumSeqs: 65})); !errors.Is(err, ErrOwnedRender) {
		t.Fatalf("out-of-range options rendered: %v", err)
	}
}

func TestRenderOwnedUnitServingRoundTripQualifies(t *testing.T) {
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
	for _, serving := range []control.ServingOptions{
		{Version: 1},
		{Version: 1, GPUMemoryPercent: 50, MaxNumSeqs: 1, MaxNumBatchedTokens: 256},
		{Version: 1, GPUMemoryPercent: 95, KVCacheDType: "auto", MaxNumSeqs: 64, MaxNumBatchedTokens: 65536, DisablePrefixCaching: true},
	} {
		p := ownedRenderProfile("vllm", "vision", &control.OwnedLaunch{ModelPath: modelDir, Port: 9100, Alias: "selected", Serving: serving})
		bound := newHardenedFixture(t).bind(*p.NativeModel)
		p.NativeModel = &bound
		raw, err := RenderOwnedUnit(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := qualifyFixtureLaunch(raw, *p.NativeModel); err != nil {
			t.Fatalf("%+v render does not qualify: %v", serving, err)
		}
	}
}

// The fingerprint covers the serving options: a changed option no longer
// matches the recorded digest, so spec verification and rollback proof
// refuse it.
func TestServingOptionsBindFingerprint(t *testing.T) {
	p := servingRenderProfile(control.ServingOptions{Version: 1})
	raw, err := RenderOwnedUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	for _, changed := range []control.ServingOptions{
		{},
		{Version: 1, GPUMemoryPercent: 80},
		{Version: 1, MaxNumSeqs: 4},
		{Version: 1, MaxNumBatchedTokens: 4096},
		{Version: 1, DisablePrefixCaching: true},
	} {
		skewed := p
		owned := *p.NativeModel.Owned
		owned.Serving = changed
		n := *p.NativeModel
		n.Owned = &owned
		skewed.NativeModel = &n
		if err := verifyOwnedSpec(skewed); !errors.Is(err, ErrOwnedRender) {
			t.Fatalf("serving %+v skew = %v", changed, err)
		}
	}
}

func TestServingFlagBounds(t *testing.T) {
	n := control.NativeModel{Runtime: "vllm", Endpoint: "http://127.0.0.1:9000", Model: "selected"}
	good := strings.TrimSpace(string(nativeLaunchFixture(t, &n)))
	for _, options := range []string{
		"--gpu-memory-utilization 0.50",
		"--gpu-memory-utilization 0.5",
		"--gpu-memory-utilization 0.9",
		"--gpu-memory-utilization 0.95",
		"--kv-cache-dtype auto",
		"--enable-prefix-caching --prefix-caching-hash-algo sha256",
		"--no-enable-prefix-caching",
		"--enable-chunked-prefill",
		"--max-num-seqs 1",
		"--max-num-seqs 64",
		"--max-num-batched-tokens 256",
		"--max-num-batched-tokens 65536",
		"--gpu-memory-utilization 0.90 --kv-cache-dtype auto --enable-chunked-prefill --enable-prefix-caching --prefix-caching-hash-algo sha256 --max-num-seqs 8 --max-num-batched-tokens 4096",
	} {
		if err := qualifyFixtureLaunch([]byte(good+" "+options+"\n"), n); err != nil {
			t.Errorf("bounded options refused: %s: %v", options, err)
		}
	}
	for _, options := range []string{
		"--gpu-memory-utilization 0.49",
		"--gpu-memory-utilization 0.96",
		"--gpu-memory-utilization 1",
		"--gpu-memory-utilization 0.900",
		"--gpu-memory-utilization .9",
		"--gpu-memory-utilization 9e-1",
		"--kv-cache-dtype fp8",
		"--kv-cache-dtype fp8_e4m3",
		"--prefix-caching-hash-algo xxhash",
		"--prefix-caching-hash-algo sha256_cbor",
		"--max-num-seqs 0",
		"--max-num-seqs 65",
		"--max-num-seqs 08",
		"--max-num-batched-tokens 255",
		"--max-num-batched-tokens 65537",
		"--max-num-batched-tokens 4k",
		"--enable-prefix-caching --no-enable-prefix-caching",
		"--enable-chunked-prefill --enable-chunked-prefill",
		"--max-num-seqs 4 --max-num-seqs 4",
		"--gpu-memory-utilization 0.90 --gpu-memory-utilization 0.90",
		"--no-enable-chunked-prefill",
		"--calculate-kv-scales",
		"--kv-offloading-size 4",
		"--max-num-seqs",
		"--gpu-memory-utilization=0.90",
	} {
		if qualifyFixtureLaunch([]byte(good+" "+options+"\n"), n) == nil {
			t.Errorf("unbounded or unknown options accepted: %s", options)
		}
	}
}

func TestServingFlagDiagnosticsNameQualifiedOptions(t *testing.T) {
	n := control.NativeModel{Runtime: "vllm", Endpoint: "http://127.0.0.1:9000", Model: "selected"}
	good := strings.TrimSpace(string(nativeLaunchFixture(t, &n)))
	err := qualifyFixtureLaunch([]byte(good+" --max-num-seqs 99\n"), n)
	if err == nil || !strings.Contains(err.Error(), "--max-num-seqs") {
		t.Fatalf("bound diagnostic = %v", err)
	}
	err = qualifyFixtureLaunch([]byte(good+" --secret-option value\n"), n)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unknown option diagnostic = %v", err)
	}
}

func servingCapacityManager(t *testing.T, outputs map[string][]byte) *SystemdManager {
	t.Helper()
	m, err := newSystemdManager(testConfig(), &fakeRunner{outputs: outputs}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCapacityUsesOwnedServingReservation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		percent  uint32
		required uint64
		headroom uint64
		free     string
		ok       bool
	}{
		// 0.90 x 24576 MiB = 22119 MiB, rounded up.
		{"default reservation fits", 0, 0, 0, "22119, 24576", true},
		{"default reservation short", 0, 0, 0, "22118, 24576", false},
		{"lower utilization fits", 50, 0, 0, "12288, 24576", true},
		{"measured requirement dominates", 50, 20000, 0, "19999, 24576", false},
		{"reservation dominates requirement", 90, 1000, 0, "22118, 24576", false},
		{"headroom added", 50, 0, 100, "12387, 24576", false},
		{"headroom fits", 50, 0, 100, "12388, 24576", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := servingCapacityManager(t, map[string][]byte{gpuFreeTotalCommand: []byte(tc.free + "\n")})
			p := &m.config.Catalog.Profiles[0]
			p.RequiredMiB = tc.required
			p.NativeModel = &control.NativeModel{Runtime: "vllm", Owned: &control.OwnedLaunch{Serving: control.ServingOptions{Version: 1, GPUMemoryPercent: tc.percent}}}
			m.config.CapacityHeadroomMiB = tc.headroom
			err := m.capacity(context.Background(), p.ID)
			if (err == nil) != tc.ok {
				t.Fatalf("capacity = %v, want ok %v", err, tc.ok)
			}
			if err != nil && (!errors.Is(err, ErrCapacity) || !strings.Contains(err.Error(), "total VRAM")) {
				t.Fatalf("capacity error = %v", err)
			}
		})
	}
}

func TestCapacityRejectsUnreadableTotal(t *testing.T) {
	for _, output := range []string{"100", "100, x", "200, 100", "0, 0", "x, 100"} {
		m := servingCapacityManager(t, map[string][]byte{gpuFreeTotalCommand: []byte(output)})
		p := &m.config.Catalog.Profiles[0]
		p.NativeModel = &control.NativeModel{Runtime: "vllm", Owned: &control.OwnedLaunch{Serving: control.ServingOptions{Version: 1}}}
		if err := m.capacity(context.Background(), p.ID); !errors.Is(err, ErrCapacity) {
			t.Fatalf("output %q = %v", output, err)
		}
	}
	m := servingCapacityManager(t, map[string][]byte{})
	m.runner.(*fakeRunner).errs = map[string]error{gpuFreeTotalCommand: errors.New("no GPU")}
	p := &m.config.Catalog.Profiles[0]
	p.NativeModel = &control.NativeModel{Runtime: "vllm", Owned: &control.OwnedLaunch{Serving: control.ServingOptions{Version: 1}}}
	if err := m.capacity(context.Background(), p.ID); !errors.Is(err, ErrCapacity) {
		t.Fatalf("query failure = %v", err)
	}
}

// writeAdoptedServingLaunch writes an adopted vLLM launch with options and
// binds its fingerprint.
func writeAdoptedServingLaunch(t *testing.T, options string) control.NativeModel {
	t.Helper()
	n := control.NativeModel{Runtime: "vllm", Endpoint: "http://127.0.0.1:9000", Model: "selected"}
	raw := []byte(strings.TrimSpace(string(nativeLaunchFixture(t, &n))) + options + "\n")
	n.LaunchFile = filepath.Join(t.TempDir(), "adopted.service")
	if err := os.WriteFile(n.LaunchFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	n.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	return n
}

func TestCapacityUsesAdoptedServingReservation(t *testing.T) {
	m := servingCapacityManager(t, map[string][]byte{gpuFreeTotalCommand: []byte("12287, 24576\n")})
	p := &m.config.Catalog.Profiles[0]
	n := writeAdoptedServingLaunch(t, " --gpu-memory-utilization 0.5 --max-num-seqs 4")
	p.NativeModel = &n
	if err := m.capacity(context.Background(), p.ID); !errors.Is(err, ErrCapacity) || !strings.Contains(err.Error(), "0.50") {
		t.Fatalf("adopted reservation short = %v", err)
	}
	m.runner.(*fakeRunner).outputs[gpuFreeTotalCommand] = []byte("12288, 24576\n")
	if err := m.capacity(context.Background(), p.ID); err != nil {
		t.Fatalf("adopted reservation fits = %v", err)
	}
	// A changed launch file no longer matches its fingerprint.
	if err := os.WriteFile(n.LaunchFile, []byte("[Service]\nExecStart=/usr/bin/vllm serve /m --gpu-memory-utilization 0.95\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.capacity(context.Background(), p.ID); !errors.Is(err, ErrCapacity) || !errors.Is(err, ErrLaunchChanged) {
		t.Fatalf("changed adopted launch = %v", err)
	}
}

func TestCapacityRefusesUnreadableAdoptedLaunch(t *testing.T) {
	missing := control.NativeModel{Runtime: "vllm", LaunchFile: filepath.Join(t.TempDir(), "missing.service"), LaunchSHA256: strings.Repeat("a", 64)}
	short := writeAdoptedServingLaunch(t, "")
	raw := []byte("[Service]\nExecStart=/usr/bin/vllm\n")
	if err := os.WriteFile(short.LaunchFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	short.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	unparsable := writeAdoptedServingLaunch(t, "")
	raw = []byte("[Service]\nExecStart=/usr/bin/vllm serve /m --unknown x\n")
	if err := os.WriteFile(unparsable.LaunchFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	unparsable.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	badUnit := writeAdoptedServingLaunch(t, "")
	raw = []byte("ExecStart\n")
	if err := os.WriteFile(badUnit.LaunchFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	badUnit.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	for name, n := range map[string]control.NativeModel{"missing": missing, "short": short, "unparsable": unparsable, "bad unit": badUnit} {
		m := servingCapacityManager(t, map[string][]byte{})
		p := &m.config.Catalog.Profiles[0]
		p.NativeModel = &n
		if err := m.capacity(context.Background(), p.ID); !errors.Is(err, ErrCapacity) {
			t.Fatalf("%s = %v", name, err)
		}
	}
}

func TestCapacityAdoptedWithoutUtilizationKeepsMeasuredRule(t *testing.T) {
	m := servingCapacityManager(t, map[string][]byte{gpuFreeCommand: []byte("100\n")})
	p := &m.config.Catalog.Profiles[0]
	n := writeAdoptedServingLaunch(t, " --max-num-seqs 4")
	p.NativeModel = &n
	if err := m.capacity(context.Background(), p.ID); err != nil {
		t.Fatalf("no requirement = %v", err)
	}
	p.RequiredMiB = 101
	if err := m.capacity(context.Background(), p.ID); !errors.Is(err, ErrCapacity) {
		t.Fatalf("measured requirement = %v", err)
	}
	for _, call := range m.runner.(*fakeRunner).calls {
		if call == gpuFreeTotalCommand {
			t.Fatal("launch without utilization queried total VRAM")
		}
	}
}

func TestReservedCapacityRejectsOverflow(t *testing.T) {
	m := servingCapacityManager(t, map[string][]byte{gpuFreeTotalCommand: []byte("100, 200\n")})
	p := &m.config.Catalog.Profiles[0]
	p.RequiredMiB = ^uint64(0)
	p.NativeModel = &control.NativeModel{Runtime: "vllm", Owned: &control.OwnedLaunch{Serving: control.ServingOptions{Version: 1}}}
	m.config.CapacityHeadroomMiB = 1
	if err := m.capacity(context.Background(), p.ID); !errors.Is(err, ErrCapacity) || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("overflow = %v", err)
	}
}

func TestHeadroomAcceptsOwnedServingReservation(t *testing.T) {
	config := SystemdConfig{Catalog: &control.Catalog{Version: 6, Profiles: []control.WorkloadProfile{
		{NativeModel: &control.NativeModel{Owned: &control.OwnedLaunch{Serving: control.ServingOptions{Version: 1}}}},
	}}}
	if !config.measuresCapacity() {
		t.Fatal("owned serving reservation does not count as a measured requirement")
	}
}
