package runtime

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPhysicalUUID = "GPU-01234567-89ab-cdef-0123-456789abcdef"

func TestQualifiedEnvironmentPreservesLiteralAssignments(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "credential-private")
	if err := os.WriteFile(token, []byte("DO-NOT-READ-TOKEN"), 0600); err != nil {
		t.Fatal(err)
	}
	names := []string{"HOME", "HF_HOME", "HUGGINGFACE_HUB_CACHE", "TRANSFORMERS_CACHE", "XDG_CACHE_HOME", "TMPDIR", "HF_TOKEN_PATH", "PATH"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			value := dir
			if name == "HF_TOKEN_PATH" {
				value = token
			}
			if name == "PATH" {
				value = "/usr/bin:/bin"
			}
			raw := []byte("[Service]\nEnvironment=\"" + name + "=" + value + "\"\nExecStart=/usr/bin/llama-server -m /models/local\n")
			u, err := parseExternalLaunchSources([][]byte{raw}, "llama.cpp")
			if err != nil || u.environmentValues[name] != value {
				t.Fatalf("qualified assignment: %v", err)
			}
			if err := CheckLoadedEnvironment(name+"="+value, u.environmentValues); err != nil {
				t.Fatal(err)
			}
			if err := CheckLoadedEnvironment(name+"=/private-different-value", u.environmentValues); err == nil || strings.Contains(err.Error(), "private-different-value") {
				t.Fatalf("loaded mutation: %v", err)
			}
			bad := strings.Replace(string(raw), value, "$PRIVATE_SECRET", 1)
			if _, err := parseExternalLaunchSources([][]byte{[]byte(bad)}, "llama.cpp"); err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "PRIVATE_SECRET") {
				t.Fatalf("unsafe diagnostic: %v", err)
			}
		})
	}
	for _, value := range []string{`"HOME=/secret`, `HOME=/private\path`, `"HF_TOKEN=PRIVATE_SECRET"`, `"PATH=.:/usr/bin"`, `"PATH=/tmp:/usr/bin"`, `"HF_TOKEN_PATH=/missing/private-token"`, `"HF_TOKEN_PATH=/"`, `"HOME=/tmp"fragment`} {
		_, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nEnvironment=" + value + "\nExecStart=/usr/bin/true x\n")}, "llama.cpp")
		if err == nil || strings.Contains(err.Error(), "PRIVATE_SECRET") || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("unsafe environment accepted or disclosed: %v", err)
		}
	}
	for _, path := range []string{dir, "/dev/null"} {
		if err := qualifyEnvironmentPath(path, true, false); err == nil {
			t.Fatal("token path accepted non-regular file", path)
		}
	}
	if err := os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	if _, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nEnvironment=HOME=" + dir + "\nExecStart=/usr/bin/true x\n")}, "llama.cpp"); err == nil {
		t.Fatal("writable HOME accepted")
	}
}

type identityRunner struct {
	output []byte
	err    error
	calls  []string
}

func (r *identityRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return r.output, r.err
}

func TestCUDAVisibilityBindsPhysicalIdentityForAdmissionAndRelease(t *testing.T) {
	raw := []byte("[Service]\nEnvironment=\"CUDA_VISIBLE_DEVICES=" + testPhysicalUUID + "\"\nExecStart=/usr/bin/true x\n")
	u, err := parseExternalLaunchSources([][]byte{raw}, "llama.cpp")
	if err != nil || u.gpuUUID != testPhysicalUUID {
		t.Fatal(err)
	}
	n := control.NativeModel{Runtime: "llama.cpp", GPUUUID: ""}
	if err := qualifyParsedLaunch(u, n, fixtureExecutableValidator); err == nil || !strings.Contains(err.Error(), "recorded physical GPU") {
		t.Fatal("omitted UUID evidence accepted", err)
	}
	for _, bad := range []string{"0", "1", "", "-1", "GPU-0123", testPhysicalUUID + ",GPU-ffffffff-ffff-ffff-ffff-ffffffffffff", "MIG-01234567-89ab-cdef-0123-456789abcdef"} {
		_, err := parseExternalLaunchSources([][]byte{[]byte("[Service]\nEnvironment=CUDA_VISIBLE_DEVICES=" + bad + "\nExecStart=/usr/bin/true x\n")}, "llama.cpp")
		if err == nil || !strings.Contains(err.Error(), "CUDA_VISIBLE_DEVICES") {
			t.Fatalf("unsafe visibility accepted: %q %v", bad, err)
		}
	}
	for _, bindingKind := range []string{"native", "comfyui"} {
		t.Run(bindingKind, func(t *testing.T) {
			p := control.WorkloadProfile{ID: "chosen", Unit: "chosen.service"}
			if bindingKind == "native" {
				p.NativeModel = &control.NativeModel{GPUUUID: testPhysicalUUID}
			} else {
				p.LaunchBinding = &control.LaunchBinding{GPUUUID: testPhysicalUUID}
			}
			r := &identityRunner{output: []byte(testPhysicalUUID + "\n")}
			m := &SystemdManager{config: SystemdConfig{GPUIndex: 2, NvidiaSMIPath: "/usr/bin/nvidia-smi", Catalog: &control.Catalog{Profiles: []control.WorkloadProfile{p}}}, runner: r}
			if err := m.verifyProfileGPU(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			if len(r.calls) != 1 || !strings.Contains(r.calls[0], "-i 2") {
				t.Fatal("did not resolve configured physical index", r.calls)
			}
			for _, failure := range []string{"GPU-ffffffff-ffff-ffff-ffff-ffffffffffff\n", testPhysicalUUID + "\n" + testPhysicalUUID, "unexpected"} {
				r.output = []byte(failure)
				r.calls = nil
				if err := m.Start(context.Background(), p.ID); err == nil {
					t.Fatal("started mismatched GPU")
				}
				if len(r.calls) != 1 || strings.Contains(r.calls[0], " start ") {
					t.Fatal("startup before GPU proof", r.calls)
				}
				if err := m.ReleasedFor(context.Background(), p.ID); err == nil {
					t.Fatal("release accepted mismatched GPU")
				}
			}
			r.err = errors.New("PRIVATE_QUERY_FAILURE")
			r.calls = nil
			if err := m.verifyProfileGPU(context.Background(), p); err == nil || strings.Contains(err.Error(), "PRIVATE_QUERY_FAILURE") {
				t.Fatal("query failure disclosed or accepted", err)
			}
		})
	}
}

func TestTrustFailuresIdentifyComponentWithoutPrivatePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private-service-name.service")
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/usr/bin/true x\n"), 0666); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0666)
	_, err := readLaunchSource(path)
	if err == nil || !strings.Contains(err.Error(), "component") || !strings.Contains(err.Error(), "write permission") || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "private-service-name") {
		t.Fatalf("unhelpful or leaking failure: %v", err)
	}
}

func TestCUDAUUIDSourceBindingCannotBeOmittedOrDrifted(t *testing.T) {
	n := control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://localhost:9000", Model: "selected", GPUUUID: testPhysicalUUID}
	raw := append(nativeLaunchFixture(t, n.Runtime, n.Endpoint, n.Model), []byte("Environment=CUDA_VISIBLE_DEVICES="+testPhysicalUUID+"\n")...)
	path := filepath.Join(t.TempDir(), "bound.service")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	n.LaunchFile = path
	launch, err := inspectAutomaticLaunch(raw, n.Runtime, fixtureExecutableValidator)
	if err != nil {
		t.Fatal(err)
	}
	n.LaunchSHA256 = launch.SHA256
	if _, err := inspectQualifiedNativeLaunch(path, n, fixtureExecutableValidator); err != nil {
		t.Fatal(err)
	}
	omitted := n
	omitted.GPUUUID = ""
	if _, err := inspectQualifiedNativeLaunch(path, omitted, fixtureExecutableValidator); err == nil {
		t.Fatal("fingerprint omitted source GPU evidence")
	}
	p := control.WorkloadProfile{ID: "chosen", Unit: "bound.service", NativeModel: &n}
	query := "/usr/bin/nvidia-smi --query-gpu=uuid --format=csv,noheader -i 2"
	show := "/usr/bin/true --user show --property=FragmentPath --property=DropInPaths --property=NeedDaemonReload --property=ExecStartPre --property=ExecStart --property=Environment -- bound.service"
	r := &fakeRunner{outputs: map[string][]byte{query: []byte(testPhysicalUUID + "\n"), show: []byte("FragmentPath=" + path + "\nDropInPaths=\nNeedDaemonReload=no\nExecStart={ path=" + strings.Fields(launch.Command)[0] + " ; argv[]=" + launch.Command + " ; }\nEnvironment=CUDA_VISIBLE_DEVICES=" + testPhysicalUUID + "\n")}}
	m := &SystemdManager{config: SystemdConfig{GPUIndex: 2, NvidiaSMIPath: "/usr/bin/nvidia-smi", SystemctlPath: "/usr/bin/true", Catalog: &control.Catalog{Profiles: []control.WorkloadProfile{p}}}, runner: r, nativeExecutableValidator: fixtureExecutableValidator}
	if err := m.verifyNativeBinding(context.Background(), p); err != nil {
		t.Fatal("physical identity binding", err)
	}
	if err := m.ReleasedFor(context.Background(), p.ID); err != nil {
		t.Fatal("release physical identity binding", err)
	}
	r.outputs[show] = []byte(strings.Replace(string(r.outputs[show]), "Environment=CUDA_VISIBLE_DEVICES="+testPhysicalUUID, "Environment=CUDA_VISIBLE_DEVICES=PRIVATE_LOADED_SECRET", 1))
	if err := m.ReleasedFor(context.Background(), p.ID); err == nil || strings.Contains(err.Error(), "PRIVATE_LOADED_SECRET") {
		t.Fatal("release accepted or disclosed loaded environment drift", err)
	}
	r.outputs[show] = []byte("FragmentPath=" + path + "\nNeedDaemonReload=no\n")
	os.WriteFile(path, []byte(strings.Replace(string(raw), testPhysicalUUID, "GPU-ffffffff-ffff-ffff-ffff-ffffffffffff", 1)), 0600)
	if err := m.ReleasedFor(context.Background(), p.ID); err == nil {
		t.Fatal("release accepted source drift")
	}
}

func TestEnvironmentRejectsAmbiguousAndUninspectedLoadedSettings(t *testing.T) {
	expected := map[string]string{"HOME": "/root"}
	for _, output := range []string{"HOME=/root HOME=/root", "HOME=/root LD_PRELOAD=/PRIVATE_LIBRARY", `"HOME=/unterminated`, "HOME=/root BAD-NAME=/private", "HOME=/root private-value-without-assignment"} {
		err := CheckLoadedEnvironment(output, expected)
		if err == nil || strings.Contains(err.Error(), "PRIVATE_LIBRARY") || strings.Contains(err.Error(), "unterminated") && !strings.Contains(err.Error(), "quote") || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("ambiguous loaded environment accepted or disclosed: %v", err)
		}
	}
	for _, output := range []string{`"PRIVATE_SECRET`, `"BAD-NAME=/private`, "BAD-NAME=/private", "NAME-without-assignment"} {
		if _, err := environmentAssignments(output); err == nil || strings.Contains(err.Error(), "PRIVATE_SECRET") || strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid name disclosed: %v", err)
		}
	}
	for _, path := range []string{"", "relative", "/missing/private-directory"} {
		if err := qualifyEnvironmentSearchPath(path); err == nil {
			t.Fatal("invalid search directory accepted")
		}
	}
	u := parsedLaunchUnit{}
	homeAssignment := "HOME=" + t.TempDir()
	if err := u.applySafeEnvironment("llama.cpp", homeAssignment); err != nil {
		t.Fatal(err)
	}
	if err := u.applySafeEnvironment("llama.cpp", homeAssignment); err == nil {
		t.Fatal("ambiguous direct assignment accepted")
	}
	if err := u.applySafeEnvironment("llama.cpp", "malformed"); err == nil {
		t.Fatal("malformed assignment accepted")
	}
	for _, path := range []string{"/usr/bin/true", "/missing/ollama"} {
		if err := ValidateSelectedNativeExecutable("ollama", path); err == nil {
			t.Fatal("unqualified executable accepted")
		}
	}
	p := control.WorkloadProfile{NativeModel: &control.NativeModel{GPUUUID: testPhysicalUUID}}
	m := &SystemdManager{}
	if err := m.verifyProfileGPU(context.Background(), p); err == nil || !strings.Contains(err.Error(), "cannot be inspected") {
		t.Fatal("absent GPU inspector accepted", err)
	}
}
