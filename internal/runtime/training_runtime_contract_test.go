package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

const trainerContractGPU = "GPU-11111111-1111-1111-1111-111111111111"
const trainerContractInvocation = "1234567890abcdef1234567890abcdef"

func trainerContractFixture(t *testing.T) (*SystemdManager, *fakeRunner, control.WorkloadProfile, control.TrainingJob, string) {
	t.Helper()
	r := stoppedRunner()
	m := strictManager(t, r)
	dir := t.TempDir()
	executable, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	binding := &control.TrainerBinding{Version: 1, GPUUUID: trainerContractGPU, Executable: "/usr/bin/true", ExecutableSHA256: fmt.Sprintf("%x", sha256.Sum256(executable)), RecipeFile: filepath.Join(dir, "recipe.json"), LaunchFile: filepath.Join(dir, "training.service"), JobFile: filepath.Join(dir, "job.json"), TerminalFile: filepath.Join(dir, "terminal.json")}
	recipe := []byte(`{"version":1}`)
	binding.RecipeSHA256 = fmt.Sprintf("%x", sha256.Sum256(recipe))
	launch := []byte(TrainerUnit(*binding))
	binding.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(launch))
	for path, raw := range map[string][]byte{binding.RecipeFile: recipe, binding.LaunchFile: launch} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := control.WorkloadProfile{ID: "training", Unit: "training.service", Cgroup: "/workloads/training.service", Trainer: binding}
	m.config.Catalog.Profiles = append(m.config.Catalog.Profiles, p)
	// Capture the observation command rather than duplicate its property ordering.
	_, _ = m.trainerProperties(context.Background(), p)
	command := r.calls[len(r.calls)-1]
	r.outputs[command] = []byte("LoadState=loaded\nActiveState=active\nSubState=exited\nControlGroup=/workloads/training.service\nInvocationID=" + trainerContractInvocation + "\nResult=success\nExecMainCode=1\nExecMainStatus=0\nFragmentPath=" + binding.LaunchFile + "\nDropInPaths=\nNeedDaemonReload=no\nExecStart={ path=/usr/bin/true ; argv[]=/usr/bin/true --job-file " + binding.JobFile + " ; }\nEnvironment=CUDA_VISIBLE_DEVICES=" + trainerContractGPU + "\nType=exec\nRestart=no\nKillMode=control-group\nRemainAfterExit=yes\n")
	r.outputs["/usr/bin/true --query-gpu=uuid --format=csv,noheader -i 0"] = []byte(trainerContractGPU + "\n")
	job := control.TrainingJob{ID: "job-one", Token: "token-one", Workload: p.ID, Fence: control.Fence{Incarnation: "session-one", Epoch: 7}, RecipeSHA256: binding.RecipeSHA256, InvocationID: trainerContractInvocation}
	return m, r, p, job, command
}

func trainerContractEvidence(job control.TrainingJob) control.TrainingTerminal {
	return control.TrainingTerminal{Version: 1, ID: job.ID, Token: job.Token, Fence: job.Fence, RecipeSHA256: job.RecipeSHA256, InvocationID: job.InvocationID, Outcome: "completed"}
}

func writeTrainerContractEvidence(t *testing.T, path string, evidence control.TrainingTerminal) {
	t.Helper()
	raw, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTrainerContractLoadedBinding(t *testing.T) {
	m, r, p, _, command := trainerContractFixture(t)
	if err := m.verifyTrainerBinding(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	good := string(r.outputs[command])
	for _, change := range [][2]string{{"Restart=no", "Restart=always"}, {"Type=exec", "Type=simple"}, {"KillMode=control-group", "KillMode=process"}, {"RemainAfterExit=yes", "RemainAfterExit=no"}, {"NeedDaemonReload=no", "NeedDaemonReload=yes"}, {"DropInPaths=", "DropInPaths=/untrusted.conf"}, {"FragmentPath=" + p.Trainer.LaunchFile, "FragmentPath=/other.service"}, {"Environment=CUDA_VISIBLE_DEVICES=" + trainerContractGPU, "Environment=CUDA_VISIBLE_DEVICES=0"}, {"argv[]=/usr/bin/true --job-file " + p.Trainer.JobFile, "argv[]=/usr/bin/true --job-file /other.json"}} {
		t.Run(change[1], func(t *testing.T) {
			r.outputs[command] = []byte(strings.Replace(good, change[0], change[1], 1))
			if err := m.verifyTrainerBinding(context.Background(), p); err == nil {
				t.Fatal("loaded launch drift accepted")
			}
		})
	}
	for _, hook := range []string{"ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost"} {
		r.outputs[command] = []byte(good + hook + "=/usr/bin/true\n")
		if err := m.verifyTrainerBinding(context.Background(), p); err == nil {
			t.Fatalf("%s hook accepted", hook)
		}
	}
	r.outputs[command] = []byte(good)
	for _, path := range []string{p.Trainer.RecipeFile, p.Trainer.LaunchFile} {
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(original, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		if err := m.verifyTrainerBinding(context.Background(), p); err == nil {
			t.Fatalf("modified source %s accepted", path)
		}
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTrainerContractCompletionRequiresFencedAcknowledgement(t *testing.T) {
	m, r, p, job, command := trainerContractFixture(t)
	if _, err := m.TrainingTerminal(context.Background(), p, job); err == nil {
		t.Fatal("exit zero without acknowledgement accepted")
	}
	good := trainerContractEvidence(job)
	writeTrainerContractEvidence(t, p.Trainer.TerminalFile, good)
	if result, err := m.TrainingTerminal(context.Background(), p, job); err != nil || result != good {
		t.Fatalf("valid completion: %#v, %v", result, err)
	}
	mutations := map[string]func(*control.TrainingTerminal){
		"old job":         func(e *control.TrainingTerminal) { e.ID = "older" },
		"old token":       func(e *control.TrainingTerminal) { e.Token = "older" },
		"old epoch":       func(e *control.TrainingTerminal) { e.Fence.Epoch-- },
		"old incarnation": func(e *control.TrainingTerminal) { e.Fence.Incarnation = "older" },
		"old recipe":      func(e *control.TrainingTerminal) { e.RecipeSHA256 = strings.Repeat("0", 64) },
		"old invocation":  func(e *control.TrainingTerminal) { e.InvocationID = strings.Repeat("a", 32) },
		"unknown version": func(e *control.TrainingTerminal) { e.Version++ },
		"unknown outcome": func(e *control.TrainingTerminal) { e.Outcome = "success" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			e := good
			mutate(&e)
			writeTrainerContractEvidence(t, p.Trainer.TerminalFile, e)
			if _, err := m.TrainingTerminal(context.Background(), p, job); err == nil {
				t.Fatal("stale evidence accepted")
			}
		})
	}
	writeTrainerContractEvidence(t, p.Trainer.TerminalFile, good)
	original := string(r.outputs[command])
	for _, change := range [][2]string{{"SubState=exited", "SubState=running"}, {"InvocationID=" + job.InvocationID, "InvocationID=" + strings.Repeat("b", 32)}, {"ExecMainStatus=0", "ExecMainStatus=1"}, {"ExecMainCode=1", "ExecMainCode=2"}, {"Result=success", "Result=signal"}} {
		r.outputs[command] = []byte(strings.Replace(original, change[0], change[1], 1))
		if _, err := m.TrainingTerminal(context.Background(), p, job); err == nil {
			t.Fatalf("conflicting process evidence accepted: %s", change[1])
		}
	}
}

func TestTrainerContractCompletionAcceptsMaximumRequestIDs(t *testing.T) {
	for name, id := range map[string]string{
		"plain":   strings.Repeat("x", control.MaxRequestIDBytes),
		"escaped": strings.Repeat("<", control.MaxRequestIDBytes),
		"UTF-8":   strings.Repeat("é", control.MaxRequestIDBytes/2),
	} {
		t.Run(name, func(t *testing.T) {
			if err := control.ValidateRequestID(id); err != nil {
				t.Fatal(err)
			}
			m, _, p, job, _ := trainerContractFixture(t)
			job.ID = id
			good := trainerContractEvidence(job)
			raw, err := json.Marshal(good)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) <= 4096 {
				t.Fatalf("receipt does not exercise the former limit: %d bytes", len(raw))
			}
			if name == "escaped" && len(raw) <= 6*control.MaxRequestIDBytes {
				t.Fatalf("receipt does not exercise six-byte escaping: %d bytes", len(raw))
			}
			for _, size := range []int{len(raw), 64 << 10} {
				body := append([]byte(nil), raw...)
				body = append(body, []byte(strings.Repeat(" ", size-len(raw)))...)
				if err := os.WriteFile(p.Trainer.TerminalFile, body, 0600); err != nil {
					t.Fatal(err)
				}
				if result, err := m.TrainingTerminal(context.Background(), p, job); err != nil || result != good {
					t.Fatalf("valid %d-byte completion: %#v, %v", size, result, err)
				}
			}
		})
	}
}
func TestTrainerContractEnvelopeDoesNotFollowExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	path := filepath.Join(dir, "job.json")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := writeTrainingEnvelope(path, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "preserve" {
		t.Fatalf("symlink target changed: %q %v", raw, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe envelope mode: %v", info.Mode())
	}
}

func TestTrainerContractReleaseRequiresCgroupAndGPUProof(t *testing.T) {
	m, r, p, _, _ := trainerContractFixture(t)
	root := fixtureCgroups(t, m)
	command := "/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- training.service"
	r.outputs[command] = stoppedOutput()
	gpuCommand := "/usr/bin/true --query-compute-apps=pid --format=csv,noheader,nounits -i " + trainerContractGPU
	writeEvents(t, root, "training.service", "populated 0\n")
	if err := m.ReleaseTraining(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	writeEvents(t, root, "training.service", "populated 1\n")
	if err := m.ReleaseTraining(context.Background(), p); err == nil {
		t.Fatal("recursive populated cgroup accepted")
	}
	writeEvents(t, root, "training.service", "populated 0\n")
	r.outputs[gpuCommand] = []byte("1234\n")
	if err := m.ReleaseTraining(context.Background(), p); err == nil {
		t.Fatal("surviving GPU process accepted")
	}
	r.outputs[gpuCommand] = nil
	if err := m.ReleaseTraining(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestTrainerContractReleaseFailsClosedOnMissingGPUOrCapacity(t *testing.T) {
	m, r, p, _, _ := trainerContractFixture(t)
	r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- training.service"] = stoppedOutput()
	gpuCommand := "/usr/bin/true --query-compute-apps=pid --format=csv,noheader,nounits -i " + trainerContractGPU
	r.errs = map[string]error{gpuCommand: fmt.Errorf("GPU observation unavailable")}
	if err := m.ReleaseTraining(context.Background(), p); err == nil {
		t.Fatal("GPU observation failure ignored")
	}
	delete(r.errs, gpuCommand)
	for i := range m.config.Catalog.Profiles {
		if m.config.Catalog.Profiles[i].ID == p.ID {
			m.config.Catalog.Profiles[i].RequiredMiB = 1024
		}
	}
	r.outputs[gpuFreeCommand] = []byte("0\n")
	if err := m.ReleaseTraining(context.Background(), p); err == nil {
		t.Fatal("insufficient post-exit GPU capacity ignored")
	}
	r.outputs[gpuFreeCommand] = []byte("65536\n")
	if err := m.ReleaseTraining(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestTrainerContractUncertainStartIsIssuedOnlyOnce(t *testing.T) {
	m, r, p, job, command := trainerContractFixture(t)
	r.outputs[command] = []byte(strings.ReplaceAll(strings.ReplaceAll(string(r.outputs[command]), "ActiveState=active", "ActiveState=inactive"), "SubState=exited", "SubState=dead"))
	r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- training.service"] = stoppedOutput()
	// No new invocation appears after the start command. This is ambiguous even
	// if systemctl itself reports success; runtime must not retry the command.
	id, err := m.StartTraining(context.Background(), p, job)
	if err == nil || id != "" {
		t.Fatalf("uncertain start accepted: %q, %v", id, err)
	}
	starts := 0
	for _, call := range r.calls {
		if call == "/usr/bin/true --user --job-mode=fail start -- training.service" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("start commands = %d, calls = %v, error = %v", starts, r.calls, err)
	}
	raw, err := os.ReadFile(p.Trainer.JobFile)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Version      int                 `json:"version"`
		Job          control.TrainingJob `json:"job"`
		RecipeFile   string              `json:"recipeFile"`
		TerminalFile string              `json:"terminalFile"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != 1 || envelope.Job.ID != job.ID || envelope.Job.Token != job.Token || envelope.Job.Fence != job.Fence || envelope.RecipeFile != p.Trainer.RecipeFile || envelope.TerminalFile != p.Trainer.TerminalFile {
		t.Fatalf("incorrect persisted launch envelope: %#v", envelope)
	}
}

func TestTrainerContractUnavailableBindingFailsClosed(t *testing.T) {
	for _, kind := range []string{"missing binding", "missing executable", "changed executable", "missing recipe", "missing unit", "manager unavailable"} {
		t.Run(kind, func(t *testing.T) {
			m, r, p, _, command := trainerContractFixture(t)
			switch kind {
			case "missing binding":
				p.Trainer = nil
			case "missing executable":
				p.Trainer.Executable = filepath.Join(t.TempDir(), "missing")
			case "changed executable":
				p.Trainer.ExecutableSHA256 = strings.Repeat("0", 64)
			case "missing recipe":
				if err := os.Remove(p.Trainer.RecipeFile); err != nil {
					t.Fatal(err)
				}
			case "missing unit":
				if err := os.Remove(p.Trainer.LaunchFile); err != nil {
					t.Fatal(err)
				}
			case "manager unavailable":
				r.errs = map[string]error{command: fmt.Errorf("manager unavailable")}
			}
			if err := m.verifyTrainerBinding(context.Background(), p); err == nil {
				t.Fatal("unverifiable trainer accepted")
			}
		})
	}
}

func TestTrainerContractStartPreconditionsPreventLaunch(t *testing.T) {
	for _, kind := range []string{"binding drift", "existing workload", "capacity", "trainer running", "unsafe job parent", "unsafe terminal parent", "job path directory"} {
		t.Run(kind, func(t *testing.T) {
			m, r, p, job, command := trainerContractFixture(t)
			r.outputs[command] = []byte(strings.ReplaceAll(strings.ReplaceAll(string(r.outputs[command]), "ActiveState=active", "ActiveState=inactive"), "SubState=exited", "SubState=dead"))
			r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- training.service"] = stoppedOutput()
			switch kind {
			case "binding drift":
				p.Trainer.ExecutableSHA256 = strings.Repeat("0", 64)
			case "existing workload":
				r.outputs[textShowCommand] = []byte("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=/workloads/text.service\n")
			case "capacity":
				for i := range m.config.Catalog.Profiles {
					if m.config.Catalog.Profiles[i].ID == p.ID {
						m.config.Catalog.Profiles[i].RequiredMiB = 1024
					}
				}
				r.outputs[gpuFreeCommand] = []byte("0\n")
			case "trainer running":
				r.outputs[command] = []byte(strings.ReplaceAll(strings.ReplaceAll(string(r.outputs[command]), "ActiveState=inactive", "ActiveState=active"), "SubState=dead", "SubState=running"))
			case "unsafe job parent":
				unsafe := filepath.Join(filepath.Dir(p.Trainer.JobFile), "unsafe")
				if err := os.Mkdir(unsafe, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(unsafe, 0777); err != nil {
					t.Fatal(err)
				}
				old := p.Trainer.JobFile
				p.Trainer.JobFile = filepath.Join(unsafe, "job.json")
				raw := []byte(TrainerUnit(*p.Trainer))
				p.Trainer.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
				if err := os.WriteFile(p.Trainer.LaunchFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
				r.outputs[command] = []byte(strings.ReplaceAll(string(r.outputs[command]), old, p.Trainer.JobFile))
			case "unsafe terminal parent":
				unsafe := filepath.Join(filepath.Dir(p.Trainer.TerminalFile), "unsafe")
				if err := os.Mkdir(unsafe, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(unsafe, 0777); err != nil {
					t.Fatal(err)
				}
				p.Trainer.TerminalFile = filepath.Join(unsafe, "terminal.json")
			case "job path directory":
				if err := os.Mkdir(p.Trainer.JobFile, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if id, err := m.StartTraining(context.Background(), p, job); err == nil || id != "" {
				t.Fatalf("start precondition ignored: %q %v", id, err)
			}
			if kind == "unsafe terminal parent" {
				if _, err := os.Lstat(p.Trainer.JobFile); !os.IsNotExist(err) {
					t.Fatalf("unsafe terminal destination caused envelope effect: %v", err)
				}
			}
			for _, call := range r.calls {
				if strings.Contains(call, " --job-mode=fail start ") {
					t.Fatalf("issued start despite %s", kind)
				}
			}
		})
	}
}

type trainerObservationRunner struct {
	*fakeRunner
	command           string
	observations      int
	failAt            int
	beforeObservation func(int)
}

func (r *trainerObservationRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	if call == r.command {
		r.observations++
		if r.beforeObservation != nil {
			r.beforeObservation(r.observations)
		}
		if r.observations == r.failAt {
			return nil, fmt.Errorf("trainer observation unavailable")
		}
		if r.observations == 4 {
			r.outputs[call] = []byte(strings.ReplaceAll(string(r.outputs[call]), trainerContractInvocation, strings.Repeat("b", 32)))
		}
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

func TestTrainerContractStartObservationFailureAndSuccess(t *testing.T) {
	for _, failAt := range []int{0, 3, 4} {
		t.Run(fmt.Sprintf("observation-%d", failAt), func(t *testing.T) {
			m, r, p, job, command := trainerContractFixture(t)
			r.outputs[command] = []byte(strings.ReplaceAll(strings.ReplaceAll(string(r.outputs[command]), "ActiveState=active", "ActiveState=inactive"), "SubState=exited", "SubState=dead"))
			r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- training.service"] = stoppedOutput()
			m.runner = &trainerObservationRunner{fakeRunner: r, command: command, failAt: failAt}
			id, err := m.StartTraining(context.Background(), p, job)
			if failAt == 0 {
				if err != nil || id != strings.Repeat("b", 32) {
					t.Fatalf("fresh invocation rejected: %q %v", id, err)
				}
			} else if err == nil || id != "" {
				t.Fatalf("missing observation accepted: %q %v", id, err)
			}
			starts := 0
			for _, call := range r.calls {
				if strings.Contains(call, " --job-mode=fail start ") {
					starts++
				}
			}
			want := 1
			if failAt == 3 {
				want = 0
			}
			if starts != want {
				t.Fatalf("start attempts=%d, want %d", starts, want)
			}
		})
	}
}

func TestTrainerContractTerminalAndReleaseRejectUnavailableIdentity(t *testing.T) {
	m, r, p, job, command := trainerContractFixture(t)
	r.errs = map[string]error{command: fmt.Errorf("manager unavailable")}
	if _, err := m.TrainingTerminal(context.Background(), p, job); err == nil {
		t.Fatal("terminal accepted without manager observation")
	}
	r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- training.service"] = stoppedOutput()
	r.outputs["/usr/bin/true --query-gpu=uuid --format=csv,noheader -i 0"] = []byte("GPU-00000000-0000-0000-0000-000000000000\n")
	if err := m.ReleaseTraining(context.Background(), p); err == nil {
		t.Fatal("release accepted for different physical GPU")
	}
}

func TestTrainerContractEnvelopeWriteFailureLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeTrainingEnvelope(path, []byte("job")); err == nil {
		t.Fatal("directory replaced with envelope")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "job.json" {
		t.Fatalf("temporary envelope leaked: %v", entries)
	}
	if err := writeTrainingEnvelope(filepath.Join(dir, "missing", "job.json"), []byte("job")); err == nil {
		t.Fatal("missing parent accepted")
	}
}

func TestTrainerContractMalformedTerminalDoesNotExposeKeys(t *testing.T) {
	m, _, p, job, _ := trainerContractFixture(t)
	const privateKey = "PRIVATE_TERMINAL_FIELD_SENTINEL"
	for _, raw := range []string{`{"PRIVATE_TERMINAL_FIELD_SENTINEL":1}`, `{"PRIVATE_TERMINAL_FIELD_SENTINEL":1,"PRIVATE_TERMINAL_FIELD_SENTINEL":2}`} {
		if err := os.WriteFile(p.Trainer.TerminalFile, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := m.TrainingTerminal(context.Background(), p, job)
		if err == nil || strings.Contains(err.Error(), privateKey) {
			t.Fatalf("unsafe terminal error: %v", err)
		}
		if strings.Contains(raw, ":2") && !errors.Is(err, strictjson.ErrDuplicateKey) {
			t.Fatalf("lost duplicate-key cause: %v", err)
		}
	}
}
func TestTrainerContractRechecksOutputsImmediatelyBeforeEnvelope(t *testing.T) {
	m, r, p, job, command := trainerContractFixture(t)
	r.outputs[command] = []byte(strings.ReplaceAll(strings.ReplaceAll(string(r.outputs[command]), "ActiveState=active", "ActiveState=inactive"), "SubState=exited", "SubState=dead"))
	r.outputs["/usr/bin/true --user show --property=LoadState --property=ActiveState --property=SubState --property=ControlGroup -- training.service"] = stoppedOutput()
	m.runner = &trainerObservationRunner{fakeRunner: r, command: command, beforeObservation: func(observation int) {
		if observation == 3 {
			// Binding verification passed, but the destination changes during the
			// final stopped-unit observation before envelope creation.
			if err := os.Mkdir(p.Trainer.JobFile, 0700); err != nil {
				t.Fatal(err)
			}
		}
	}}
	if id, err := m.StartTraining(context.Background(), p, job); !errors.Is(err, ErrLaunchUnsupported) || id != "" {
		t.Fatalf("changed destination accepted: %q %v", id, err)
	}
	for _, call := range r.calls {
		if strings.Contains(call, " --job-mode=fail start ") {
			t.Fatal("changed destination reached systemctl start")
		}
	}
}
