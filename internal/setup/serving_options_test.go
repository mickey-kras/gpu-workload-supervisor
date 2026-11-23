package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

// servingHelpFixture is an excerpt of `vllm serve --help=all` of vLLM
// 0.14.1 in argparse format.
const servingHelpFixture = `usage: vllm serve [model_tag] [options]
                  [--kv-cache-dtype {auto,bfloat16,fp8}] [--max-num-seqs MAX_NUM_SEQS]

CacheConfig:
  --gpu-memory-utilization GPU_MEMORY_UTILIZATION
                        The fraction of GPU memory to be used for the model
                        executor. (default: 0.9)
  --kv-cache-dtype {auto,bfloat16,fp8,fp8_e4m3,fp8_e5m2,fp8_inc,fp8_ds_mla}
                        Data type for kv cache storage. (default: auto)
  --enable-prefix-caching, --no-enable-prefix-caching
                        Whether to enable prefix caching. (default: None)
  --prefix-caching-hash-algo {sha256,sha256_cbor,xxhash,xxhash_cbor}
                        Set the hash algorithm for prefix caching.
                        --not-an-entry appears only in a description.

SchedulerConfig:
  --enable-chunked-prefill, --no-enable-chunked-prefill
                        If True, prefill requests can be chunked. (default: None)
  --max-num-batched-tokens MAX_NUM_BATCHED_TOKENS
                        Maximum number of tokens. (default: None)
  --max-num-seqs MAX_NUM_SEQS  Maximum number of sequences. (default: None)
`

func servingDraft(id string, port uint16, serving control.ServingOptions) Draft {
	d := ownedDraft(id, port)
	d.Binding.Owned.Serving = serving
	return d
}

func servingFixtureProfile(t *testing.T, home string, serving control.ServingOptions) (control.WorkloadProfile, []byte) {
	t.Helper()
	profile, raw, err := OwnedProfile(servingDraft("vision", 9100, serving), "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	return profile, raw
}

// servingCommand answers the vLLM help with help and every other command
// like fakeOwnedCommandFor. It records the help runs.
func servingCommand(home, help string, helpErr error, runs *[]string) func(context.Context, string, ...string) ([]byte, error) {
	owned := fakeOwnedCommandFor(home)
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if slices.Equal(args, servingHelpArgs) {
			*runs = append(*runs, name)
			return []byte(help), helpErr
		}
		return owned(ctx, name, args...)
	}
}

func TestParseServingHelp(t *testing.T) {
	help := parseServingHelp([]byte(servingHelpFixture))
	for _, option := range []string{"--gpu-memory-utilization", "--kv-cache-dtype", "--enable-prefix-caching", "--no-enable-prefix-caching", "--prefix-caching-hash-algo", "--enable-chunked-prefill", "--max-num-batched-tokens", "--max-num-seqs"} {
		if _, ok := help[option]; !ok {
			t.Fatalf("option %s not parsed from %v", option, help)
		}
	}
	if _, ok := help["--not-an-entry"]; ok {
		t.Fatal("description text parsed as an option")
	}
	if !slices.Equal(help["--prefix-caching-hash-algo"], []string{"sha256", "sha256_cbor", "xxhash", "xxhash_cbor"}) || help["--max-num-seqs"] != nil {
		t.Fatalf("choices = %v", help)
	}
	if err := help.check(control.ServingOptions{Version: 1, MaxNumSeqs: 8, MaxNumBatchedTokens: 4096}.Args()); err != nil {
		t.Fatalf("supported options refused: %v", err)
	}
	if err := help.check(control.ServingOptions{Version: 1, DisablePrefixCaching: true}.Args()); err != nil {
		t.Fatalf("diagnostic option refused: %v", err)
	}
}

func TestServingHelpRefusesMissingOptionsAndValues(t *testing.T) {
	without := strings.Replace(servingHelpFixture, "  --prefix-caching-hash-algo {sha256,sha256_cbor,xxhash,xxhash_cbor}\n", "", 1)
	if err := parseServingHelp([]byte(without)).check(control.ServingOptions{Version: 1}.Args()); err == nil || !strings.Contains(err.Error(), "--prefix-caching-hash-algo") {
		t.Fatalf("missing option = %v", err)
	}
	noSHA := strings.Replace(servingHelpFixture, "{sha256,sha256_cbor,xxhash,xxhash_cbor}", "{xxhash,xxhash_cbor}", 1)
	if err := parseServingHelp([]byte(noSHA)).check(control.ServingOptions{Version: 1}.Args()); err == nil || !strings.Contains(err.Error(), "does not accept sha256") {
		t.Fatalf("missing choice = %v", err)
	}
	if err := (servingHelp{}).check([]string{"--unknown"}); err == nil {
		t.Fatal("unknown option accepted")
	}
}

func TestCheckServingOptionsRunsBoundedHelp(t *testing.T) {
	backend, home, _ := fixture(t)
	profile, _ := servingFixtureProfile(t, home, control.ServingOptions{Version: 1, MaxNumSeqs: 8})
	second := profile
	second.ID = "second"
	catalog := control.Catalog{Version: control.CatalogVersionArtifactSandbox, Profiles: []control.WorkloadProfile{profile, second}}
	var runs []string
	backend.runCommand = servingCommand(home, servingHelpFixture, nil, &runs)
	if err := backend.checkServingOptions(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0] != gpuruntime.OwnedExecutable(*profile.NativeModel) {
		t.Fatalf("help runs = %q, want one run of the owned executable", runs)
	}
	for name, tc := range map[string]struct {
		help string
		err  error
	}{
		"failed run":     {servingHelpFixture, errors.New("exit status 1")},
		"cut output":     {servingHelpFixture + strings.Repeat("x", commandOutputLimit), nil},
		"no options":     {"usage: vllm serve\n", nil},
		"missing option": {strings.Replace(servingHelpFixture, "--max-num-seqs MAX_NUM_SEQS  Maximum", "Maximum", 1), nil},
	} {
		runs = nil
		backend.runCommand = servingCommand(home, tc.help, tc.err, &runs)
		if err := backend.checkServingOptions(context.Background(), catalog); !errors.Is(err, ErrServingOptionUnsupported) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	// Launches without serving options run no help.
	plain, _ := ownedFixtureProfile(t, home, "plain", 9200)
	runs = nil
	if err := backend.checkServingOptions(context.Background(), control.Catalog{Version: control.CatalogVersionArtifactSandbox, Profiles: []control.WorkloadProfile{plain}}); err != nil || len(runs) != 0 {
		t.Fatalf("plain launch = %v, runs %q", err, runs)
	}
	backend.runCommand = nil
	if err := backend.checkServingOptions(context.Background(), catalog); !errors.Is(err, ErrServingOptionUnsupported) {
		t.Fatalf("missing runner = %v", err)
	}
}

func TestOwnedProfileRendersServingOptions(t *testing.T) {
	home := t.TempDir()
	profile, raw := servingFixtureProfile(t, home, control.ServingOptions{Version: 1})
	if profile.NativeModel.Owned.Serving != (control.ServingOptions{Version: 1}) || profile.NativeModel.LaunchSHA256 != digest(raw) {
		t.Fatalf("serving options or fingerprint lost: %+v", profile.NativeModel.Owned)
	}
	if !strings.Contains(string(raw), " --gpu-memory-utilization 0.90 --kv-cache-dtype auto --enable-chunked-prefill --enable-prefix-caching --prefix-caching-hash-algo sha256\n") {
		t.Fatalf("defaults not rendered:\n%s", raw)
	}
	if _, _, err := OwnedProfile(servingDraft("vision", 9100, control.ServingOptions{Version: 1, KVCacheDType: "fp8"}), "/user.slice/user-1000.slice/user@1000.service", home); err == nil {
		t.Fatal("unqualified fp8 KV cache accepted")
	}
	if err := validateDraftBinding(servingDraft("vision", 9100, control.ServingOptions{Version: 1, GPUMemoryPercent: 96}).Binding); err == nil {
		t.Fatal("draft accepted an out-of-range utilization")
	}
}

// An owned update with serving options is rolled back to the committed unit
// when apply fails, and a launch whose vLLM lacks an option is refused before
// any unit changes.
func TestApplyServingOptionsRollbackAndHelpRefusal(t *testing.T) {
	backend, home, r := fixture(t)
	var runs []string
	backend.runCommand = servingCommand(home, servingHelpFixture, nil, &runs)
	ctx := context.Background()
	plain, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: control.CatalogVersionArtifactSandbox, Profiles: []control.WorkloadProfile{plain}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(ownedUnitDirectory(home), plain.Unit)
	served, servedRaw := servingFixtureProfile(t, home, control.ServingOptions{Version: 1, MaxNumSeqs: 4})
	r.Catalog = control.Catalog{Version: control.CatalogVersionArtifactSandbox, Profiles: []control.WorkloadProfile{served}}
	r.ExpectedRevision = currentRevision(t, r)
	backend.makeRuntime = func(Request) (supervisor.Runtime, error) { return idleRuntime{err: errors.New("busy")}, nil }
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("non-quiescent workload applied")
	}
	backend.makeRuntime = func(Request) (supervisor.Runtime, error) { return idleRuntime{}, nil }
	if data, err := os.ReadFile(unitPath); err != nil || string(data) != string(raw) {
		t.Fatalf("committed unit not restored: %v", err)
	}
	// The installed vLLM lacks --max-num-seqs: apply refuses and the
	// committed unit stays.
	without := strings.Replace(servingHelpFixture, "--max-num-seqs MAX_NUM_SEQS  Maximum", "Maximum", 1)
	backend.runCommand = servingCommand(home, without, nil, &runs)
	if err := backend.Apply(ctx, home, r); !errors.Is(err, ErrServingOptionUnsupported) {
		t.Fatalf("unsupported option applied: %v", err)
	}
	if data, err := os.ReadFile(unitPath); err != nil || string(data) != string(raw) {
		t.Fatalf("unit changed after refusal: %v", err)
	}
	backend.runCommand = servingCommand(home, servingHelpFixture, nil, &runs)
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatalf("supported serving options refused: %v", err)
	}
	if data, err := os.ReadFile(unitPath); err != nil || string(data) != string(servedRaw) {
		t.Fatalf("serving unit not written: %v", err)
	}
}

// A drafts file with serving options is version 3, with or without limits,
// and returns to the earlier version and bytes when they are removed.
func TestDraftsVersionFollowsServingOptions(t *testing.T) {
	home := t.TempDir()
	plain := Draft{ID: "vision", Label: "Vision", App: "vllm", Binding: &DraftBinding{Instance: "owned", Owned: &DraftOwnedLaunch{ModelPath: "/m", Port: 9100}}}
	first, err := SaveDrafts(home, DraftRequest{Version: 1, Drafts: []Draft{plain}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config/gpu-workload-supervisor/drafts.json")
	plainBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	served := plain
	served.ID, served.Label = "served", "Served"
	owned := *plain.Binding.Owned
	owned.Port = 9101
	owned.Serving = control.ServingOptions{Version: 1, MaxNumSeqs: 8}
	owned.Limits = control.ResourceLimits{Version: 1, MemoryMaxMiB: 10240}
	served.Binding = &DraftBinding{Instance: "owned", Owned: &owned}
	for _, version := range []int{1, 2} {
		if _, err := SaveDrafts(home, DraftRequest{Version: version, ExpectedRevision: first.Revision, Drafts: []Draft{served}}); err == nil {
			t.Fatalf("version %d with serving options accepted", version)
		}
	}
	second, err := SaveDrafts(home, DraftRequest{Version: 3, ExpectedRevision: first.Revision, Drafts: []Draft{plain, served}})
	if err != nil || second.Version != 3 || second.Drafts[1].Binding.Owned.Serving != owned.Serving {
		t.Fatalf("save with serving options = %+v, %v", second, err)
	}
	read, err := ReadDrafts(home)
	if err != nil || read.Version != 3 {
		t.Fatalf("read with serving options = %+v, %v", read, err)
	}
	third, err := SaveDrafts(home, DraftRequest{Version: 1, ExpectedRevision: second.Revision, Drafts: []Draft{plain}})
	if err != nil || third.Version != 1 {
		t.Fatalf("save after removing serving options = %+v, %v", third, err)
	}
	if back, err := os.ReadFile(path); err != nil || string(back) != string(plainBytes) {
		t.Fatalf("bytes after removing serving options differ: %s", back)
	}
	bad := served
	badOwned := owned
	badOwned.Serving.MaxNumSeqs = 65
	bad.Binding = &DraftBinding{Instance: "owned", Owned: &badOwned}
	if err := validateDrafts(3, []Draft{bad}); err == nil || !strings.Contains(err.Error(), "draft serving options") {
		t.Fatalf("invalid serving options = %v", err)
	}
}
