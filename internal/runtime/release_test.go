package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// Fixtures emulate kernel evidence, not a list of PIDs. populated is recursive.
func fixtureCgroups(t *testing.T, m *SystemdManager) string {
	t.Helper()
	root := t.TempDir()
	m.cgroups = cgroupFS{root: root, verify: func(int) error { return nil }}
	writeEvents(t, root, "workloads", "populated 1\n")
	return filepath.Join(root, "workloads")
}

func writeEvents(t *testing.T, root, group, value string) {
	t.Helper()
	path := filepath.Join(root, group)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stoppedRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string][]byte{textShowCommand: stoppedOutput(), mediaShowCommand: stoppedOutput()}}
}

func TestReleaseUsesRecursiveCgroupsNotDesktopMemoryOrReusedPIDs(t *testing.T) {
	for _, adapter := range []string{"systemd", control.AdapterMediaUnload} {
		t.Run(adapter, func(t *testing.T) {
			r := stoppedRunner()
			// GPU accounting can be unavailable, or describe an unrelated process with
			// a reused PID. It is deliberately never part of this proof.
			r.errs = map[string]error{gpuMemoryCommand: errors.New("accounting unavailable")}
			m := strictManager(t, r)
			m.config.Catalog.Profiles[1].Adapter = adapter
			if adapter == control.AdapterMediaUnload {
				m.config.Catalog.Profiles[1].ReleaseURL = "http://127.0.0.1:8188/free"
			}
			root := fixtureCgroups(t, m)
			writeEvents(t, root, "text.service", "populated 0\nfrozen 0\n")
			writeEvents(t, root, "media.service", "populated 0\n")
			if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err != nil {
				t.Fatal(err)
			}
			writeEvents(t, root, "text.service", "populated 1\n")
			if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err == nil {
				t.Fatal("surviving descendant accepted")
			}
			writeEvents(t, root, "text.service", "populated 0\n")
			if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err != nil {
				t.Fatal(err)
			}
			for _, call := range r.calls {
				if strings.Contains(call, "query-") {
					t.Fatal(call)
				}
			}
		})
	}
}

func TestFreshManagerCanVerifyRemovedConfiguredCgroups(t *testing.T) {
	m := strictManager(t, stoppedRunner())
	fixtureCgroups(t, m)
	if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err != nil {
		t.Fatal(err)
	}
}

func TestCgroupEvidenceFailsClosed(t *testing.T) {
	for _, evidence := range []string{"", "frozen 0\n", "populated 2\n", "populated 0 extra\n", "populated 0\npopulated 1\n", "populated 1\n"} {
		t.Run(evidence, func(t *testing.T) {
			m := strictManager(t, stoppedRunner())
			root := fixtureCgroups(t, m)
			writeEvents(t, root, "text.service", evidence)
			if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err == nil {
				t.Fatal("ambiguous evidence accepted")
			}
		})
	}
	for _, kind := range []string{"missing events", "unreadable events", "symlink group", "symlink events", "non-directory", "unverifiable hierarchy", "missing hierarchy"} {
		t.Run(kind, func(t *testing.T) {
			m := strictManager(t, stoppedRunner())
			root := fixtureCgroups(t, m)
			path := filepath.Join(root, "text.service")
			switch kind {
			case "missing events":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "unreadable events":
				if err := os.MkdirAll(filepath.Join(path, "cgroup.events"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink group":
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			case "symlink events":
				writeEvents(t, root, "text.service", "populated 0\n")
				if err := os.Remove(filepath.Join(path, "cgroup.events")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/dev/null", filepath.Join(path, "cgroup.events")); err != nil {
					t.Fatal(err)
				}
			case "non-directory":
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "unverifiable hierarchy":
				m.cgroups.verify = func(int) error { return errors.New("not cgroup2 or inaccessible") }
			case "missing hierarchy":
				m.cgroups.root = filepath.Join(root, "missing")
			}
			if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err == nil {
				t.Fatal("unverifiable cgroup accepted")
			}
		})
	}
}

func TestReleaseRejectsMissingOrMismatchedCgroupMetadata(t *testing.T) {
	for _, metadata := range []string{"", "ControlGroup=/elsewhere/text.service\n", "ControlGroup=relative\n", "ControlGroup=\nControlGroup=\n"} {
		r := stoppedRunner()
		r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=inactive\nSubState=dead\n" + metadata)
		m := strictManager(t, r)
		if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err == nil {
			t.Fatalf("accepted metadata %q", metadata)
		}
	}
}

func TestUnloadSuccessResponseIsNotReleaseEvidence(t *testing.T) {
	r := stoppedRunner()
	r.outputs[mediaShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/media.service\n")
	requested := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	config := testConfig()
	config.Catalog.Profiles[1].ReleaseURL = server.URL
	m, err := newSystemdManager(config, r, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	fixtureCgroups(t, m)
	if err := m.Stop(context.Background(), control.WorkloadMedia); err != nil {
		t.Fatalf("release request: %v", err)
	}
	select {
	case <-requested:
	default:
		t.Fatal("release endpoint was not called")
	}
	if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); !errors.Is(err, ErrUnloadUnverified) || !strings.Contains(err.Error(), "stop-service") {
		t.Fatalf("error = %v", err)
	}
}

func TestCapacityIsSeparateFromReleaseAndTargetSpecific(t *testing.T) {
	r := stoppedRunner()
	r.outputs[gpuFreeCommand] = []byte("109\n")
	m := strictManager(t, r)
	m.config.Catalog.Profiles[0].RequiredMiB = 100
	m.config.Catalog.Profiles[1].RequiredMiB = 200
	m.config.CapacityHeadroomMiB = 10
	if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err != nil {
		t.Fatal(err)
	}
	rejected := func(target control.Workload) {
		t.Helper()
		r.calls = nil
		if err := m.Start(context.Background(), target); !errors.Is(err, ErrCapacity) {
			t.Fatalf("capacity = %v", err)
		}
		for _, call := range r.calls {
			if strings.Contains(call, "--user start") {
				t.Fatalf("capacity failure started runtime: %s", call)
			}
		}
	}
	rejected(control.WorkloadText)
	// Delayed driver cleanup changes available capacity, never release evidence.
	r.outputs[gpuFreeCommand] = []byte("110\n")
	if err := m.Start(context.Background(), control.WorkloadText); err != nil {
		t.Fatal(err)
	}
	rejected(control.WorkloadMedia)
	r.outputs[gpuFreeCommand] = []byte("210\n")
	if err := m.Start(context.Background(), control.WorkloadMedia); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"N/A", "1\n2", ""} {
		r.outputs[gpuFreeCommand] = []byte(value)
		rejected(control.WorkloadText)
	}
	r.errs = map[string]error{gpuFreeCommand: errors.New("GPU unavailable")}
	rejected(control.WorkloadText)
}

func TestCgroupMountMappingAndFilesystemType(t *testing.T) {
	valid := "1 2 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n"
	if err := verifyCgroupMount(valid); err != nil {
		t.Fatal(err)
	}
	for _, mounts := range []string{"", valid + valid, valid + "2 3 0:1 /subtree /sys/fs/cgroup/nested rw - cgroup2 cgroup rw\n", strings.Replace(valid, "0:1 / ", "0:1 /subtree ", 1), strings.Replace(valid, "cgroup2", "tmpfs", 1), "1 2 0:1 / /sys/fs/cgroup rw"} {
		if err := verifyCgroupMount(mounts); err == nil {
			t.Fatalf("accepted %q", mounts)
		}
	}
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := verifyCgroup2(int(file.Fd())); err == nil {
		t.Fatal("accepted ordinary directory")
	}
	if err := verifyUnifiedHierarchy(-1); err == nil {
		t.Fatal("accepted invalid descriptor")
	}
}

func TestCgroupConfigurationValidation(t *testing.T) {
	for _, group := range []string{"", "/", "relative", "/a/../b", "/a//b", "/a/", "/a\n"} {
		config := testConfig()
		config.Catalog.Profiles[0].Cgroup = group
		if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil {
			t.Fatalf("accepted %q", group)
		}
	}
	for _, group := range []string{"/workloads/text.service", "/workloads/text.service/child"} {
		config := testConfig()
		config.Catalog.Profiles[1].Cgroup = group
		if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil {
			t.Fatalf("accepted overlapping %q", group)
		}
	}
	for _, change := range []func(*SystemdConfig){
		func(c *SystemdConfig) { c.Catalog.Profiles[0].RequiredMiB = ^uint64(0); c.CapacityHeadroomMiB = 1 },
		func(c *SystemdConfig) { c.Catalog.Profiles[0].RequiredMiB = 1; c.NvidiaSMIPath = "" },
	} {
		config := testConfig()
		change(&config)
		if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err == nil {
			t.Fatal("accepted invalid capacity configuration")
		}
	}
	config := testConfig()
	config.NvidiaSMIPath = ""
	if _, err := newSystemdManager(config, &fakeRunner{}, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
}

func TestReadinessRejectsOpposingSurvivingChildrenDuringRecovery(t *testing.T) {
	for _, adapter := range []string{control.AdapterMediaUnload, "systemd"} {
		for _, target := range []control.Workload{control.WorkloadText, control.WorkloadMedia} {
			m := strictManager(t, stoppedRunner())
			m.config.Catalog.Profiles[1].Adapter = adapter
			if adapter == control.AdapterMediaUnload {
				m.config.Catalog.Profiles[1].ReleaseURL = "http://127.0.0.1:8188/free"
			}
			root := fixtureCgroups(t, m)
			opposing := "text.service"
			if target == control.WorkloadText {
				opposing = "media.service"
			}
			writeEvents(t, root, opposing, "populated 1\n")
			if err := m.Healthy(context.Background(), target); err == nil || !strings.Contains(err.Error(), "descendants") {
				t.Fatalf("%s %s: %v", adapter, target, err)
			}
		}
	}
}

func TestUnloadTextToLiveMediaVerifiesOnlyOutgoingText(t *testing.T) {
	r := stoppedRunner()
	r.outputs[mediaShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/media.service\n")
	m, err := newSystemdManager(testConfig(), r, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	root := fixtureCgroups(t, m)
	writeEvents(t, root, "text.service", "populated 1\n")
	if err := m.Start(context.Background(), control.WorkloadMedia); err == nil {
		t.Fatal("surviving text child allowed media start")
	}
	writeEvents(t, root, "text.service", "populated 0\n")
	if err := m.ReleasedFor(context.Background(), control.WorkloadMedia); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), control.WorkloadMedia); err != nil {
		t.Fatal(err)
	}
	if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); !errors.Is(err, ErrUnloadUnverified) {
		t.Fatalf("idle accepted live media: %v", err)
	}
	if err := m.ReleasedFor(context.Background(), "unknown"); err == nil {
		t.Fatal("invalid target accepted")
	}
}

func TestManagerCgroupAnchorMustExistAndMatchBothWorkloads(t *testing.T) {
	for _, metadata := range []string{
		"LoadState=loaded\nActiveState=active\nSubState=active\n",
		"LoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=\n",
		"LoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=/workloads\nControlGroup=/workloads\n",
		"LoadState=loaded\nActiveState=inactive\nSubState=dead\nControlGroup=/workloads\n",
		"LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads\n",
		"LoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=/\n",
		"LoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=/elsewhere\n",
	} {
		r := stoppedRunner()
		r.outputs[rootShowCommand] = []byte(metadata)
		m := strictManager(t, r)
		if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err == nil {
			t.Fatalf("accepted root metadata %q", metadata)
		}
	}
	for _, evidence := range []string{"missing", "missing events", "malformed"} {
		m := strictManager(t, stoppedRunner())
		anchor := filepath.Join(m.cgroups.root, "workloads")
		if err := os.Remove(filepath.Join(anchor, "cgroup.events")); err != nil {
			t.Fatal(err)
		}
		if evidence == "missing" {
			if err := os.Remove(anchor); err != nil {
				t.Fatal(err)
			}
		} else if evidence == "malformed" {
			writeEvents(t, m.cgroups.root, "workloads", "populated unknown\n")
		}
		if err := m.ReleasedFor(context.Background(), control.WorkloadIdle); err == nil {
			t.Fatalf("accepted %s manager anchor", evidence)
		}
	}
}

// Hermetic coverage of the production hierarchy probe wiring: the filesystem
// check runs first, mountinfo failures propagate, and mount mappings decide.
func TestHierarchyVerificationCombinesFilesystemAndMountEvidence(t *testing.T) {
	valid := "1 2 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n"
	accept := func(int) error { return nil }
	readMounts := func() (string, error) { return valid, nil }
	if err := verifyHierarchy(-1, accept, readMounts); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("probe failure")
	if err := verifyHierarchy(-1, func(int) error { return sentinel }, readMounts); !errors.Is(err, sentinel) {
		t.Fatalf("filesystem check bypassed: %v", err)
	}
	if err := verifyHierarchy(-1, accept, func() (string, error) { return "", sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("mountinfo failure ignored: %v", err)
	}
	if err := verifyHierarchy(-1, accept, func() (string, error) { return "", nil }); err == nil {
		t.Fatal("missing unified root accepted")
	}
	if err := requireCgroup2(unix.CGROUP2_SUPER_MAGIC); err != nil {
		t.Fatal(err)
	}
	if err := requireCgroup2(unix.TMPFS_MAGIC); err == nil {
		t.Fatal("non-cgroup2 filesystem accepted")
	}
	if _, err := selfMountinfo(); err != nil {
		t.Fatal(err)
	}
}

// Uses the production hierarchy probe. It must work for the ordinary user who
// owns the systemd --user session, without ptrace access to root-owned PID 1.
func TestUnifiedHierarchyProbeNeedsNoPrivilegedProcAccess(t *testing.T) {
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCgroupMount(string(mounts)); err != nil {
		t.Skipf("host unified cgroup root unavailable: %v", err)
	}
	root, err := os.Open("/sys/fs/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := verifyUnifiedHierarchy(int(root.Fd())); err != nil {
		t.Fatal(err)
	}
}
