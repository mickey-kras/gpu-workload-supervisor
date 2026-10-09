package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

type temporaryIdentityRunner struct {
	base           *temporaryUnitRunner
	uuid           string
	queryErr       error
	failAfterStart bool
	queries        int
}

func (r *temporaryIdentityRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if strings.Contains(strings.Join(args, " "), "--query-gpu=uuid") {
		r.queries++
		if !strings.Contains(strings.Join(args, " "), "-i 4") {
			return nil, errors.New("wrong physical index")
		}
		if r.failAfterStart && r.base.active {
			return nil, errors.New("GPU query unavailable after accepted start")
		}
		return []byte(r.uuid + "\n"), r.queryErr
	}
	return r.base.Run(ctx, name, args...)
}
func temporaryEnvironmentFixture(t *testing.T) (*SystemdManager, *temporaryIdentityRunner, control.TemporaryDiscoveryCandidate) {
	t.Helper()
	m, base, v := temporaryManagerFixture(t)
	home := t.TempDir()
	raw, err := os.ReadFile(v.LaunchFile)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("Environment=\"CUDA_VISIBLE_DEVICES="+testPhysicalUUID+"\"\nEnvironment=\"HOME="+home+"\"\n")...)
	if err := os.WriteFile(v.LaunchFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	u, err := parseExternalLaunchSources([][]byte{raw}, "ollama")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := inspectParsedAutomaticLaunch(u, raw, "ollama", m.nativeExecutableValidator)
	if err != nil {
		t.Fatal(err)
	}
	v.LaunchSHA256 = launch.SHA256
	v.GPUUUID = launch.GPUUUID
	base.mutations = map[string]string{"Environment": "OLLAMA_NO_CLOUD=1 OLLAMA_HOST=127.0.0.1:11434 CUDA_VISIBLE_DEVICES=" + testPhysicalUUID + " HOME=" + home}
	r := &temporaryIdentityRunner{base: base, uuid: testPhysicalUUID}
	m.runner = r
	m.config.NvidiaSMIPath = "/usr/bin/nvidia-smi"
	m.config.GPUIndex = 4
	return m, r, v
}
func TestTemporaryDiscoveryRequiresPhysicalAndLoadedEnvironmentProof(t *testing.T) {
	for _, mode := range []string{"wrong-physical-GPU", "missing-persisted-UUID", "loaded-CUDA-drift", "loaded-HOME-drift", "query-failure"} {
		t.Run(mode, func(t *testing.T) {
			m, r, v := temporaryEnvironmentFixture(t)
			switch mode {
			case "wrong-physical-GPU":
				r.uuid = "GPU-ffffffff-ffff-ffff-ffff-ffffffffffff"
			case "missing-persisted-UUID":
				v.GPUUUID = ""
			case "loaded-CUDA-drift":
				r.base.mutations["Environment"] = strings.ReplaceAll(r.base.mutations["Environment"], testPhysicalUUID, "GPU-ffffffff-ffff-ffff-ffff-ffffffffffff")
			case "loaded-HOME-drift":
				r.base.mutations["Environment"] = "HOME=/PRIVATE_DIRECTORY CUDA_VISIBLE_DEVICES=" + testPhysicalUUID
			case "query-failure":
				r.queryErr = errors.New("PRIVATE_QUERY_RESPONSE")
			}
			_, err := m.StartTemporaryDiscovery(context.Background(), v)
			if err == nil || r.base.starts != 0 || r.base.stops != 0 || strings.Contains(err.Error(), "PRIVATE_DIRECTORY") || strings.Contains(err.Error(), "PRIVATE_QUERY_RESPONSE") {
				t.Fatalf("unproven temporary launch admitted or disclosed: %v %+v", err, r.base)
			}
		})
	}
}
func TestTemporaryCleanupStillStopsExactInvocationAfterAdmissionProofFailure(t *testing.T) {
	for _, mode := range []string{"physical-query-after-start", "loaded-env-after-start"} {
		t.Run(mode, func(t *testing.T) {
			m, r, v := temporaryEnvironmentFixture(t)
			r.failAfterStart = mode == "physical-query-after-start"
			e, err := m.StartTemporaryDiscovery(context.Background(), v)
			if e.InvocationID == "" || r.base.starts != 1 {
				t.Fatalf("accepted invocation evidence lost: %+v %v", e, err)
			}
			if mode == "physical-query-after-start" && err == nil {
				t.Fatal("post-start GPU observation failure ignored")
			}
			if mode == "loaded-env-after-start" {
				if err != nil {
					t.Fatal(err)
				}
				r.base.mutations["Environment"] = "HOME=/PRIVATE_DIRECTORY"
			}
			if err := m.StopTemporaryDiscovery(context.Background(), v, e.InvocationID); err != nil || r.base.stops != 1 || r.base.active {
				t.Fatalf("admission proof failure prevented exact cleanup: %v %+v", err, r.base)
			}
		})
	}
}
