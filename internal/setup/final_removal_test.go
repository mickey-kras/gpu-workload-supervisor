package setup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func TestFinalApplicationUIRequestsApplyAndReopenWithRealBackend(t *testing.T) {
	backend, home, request := fixture(t)
	ctx := context.Background()
	request.Catalog.Profiles[0].ID = "comfyui"
	request.Catalog.Profiles[0].LaunchBinding = &control.LaunchBinding{Runtime: "comfyui", Endpoint: "http://127.0.0.1:8188", LaunchFile: filepath.Join(home, "application/unit"), LaunchSHA256: digest([]byte("external unit"))}
	request.Catalog.Profiles[0].HealthURL = "http://127.0.0.1:8188/system_stats"
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatal(err)
	}
	before, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = before.Revision
	request.ConfirmQuiesced = false
	paths := []string{request.Catalog.Profiles[0].LaunchBinding.LaunchFile, filepath.Join(home, "models/model.gguf"), filepath.Join(home, "application/workflow.json"), filepath.Join(home, ".config/systemd/user/comfyui.service.d/override.conf")}
	for _, path := range paths {
		acceptanceWrite(t, path, []byte("preserved application content"))
	}
	input, _ := json.Marshal(request)
	script := `
import fs from 'node:fs';
import {launch} from '../../clients/setup/harness.mjs';
const request = JSON.parse(fs.readFileSync(0, 'utf8'));
const ui = await launch({responses: {discover: {request, units: []}}, deferAction: 'unused'});
ui.edit(ui.by('Use ComfyUI'), 'active', false);
await ui.by('Continue').emit('clicked');
if (!ui.by('Finish setup').sensitive) throw new Error('Removal did not reach Finish');
await ui.by('Finish setup').emit('clicked');
process.stdout.write(JSON.stringify(ui.calls.filter(call => ['validate', 'verify-bindings', 'apply'].includes(call.argv[1])).map(call => ({action: call.argv[1], request: JSON.parse(call.input)}))));
`
	command := exec.CommandContext(t.Context(), "node", "--experimental-vm-modules", "--input-type=module", "-e", script)
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("UI removal failed: %v: %s", err, stderr.String())
	}
	var phases []struct {
		Action  string  `json:"action"`
		Request Request `json:"request"`
	}
	if err := json.Unmarshal(output, &phases); err != nil {
		t.Fatal(err)
	}
	if len(phases) != 3 {
		t.Fatalf("missing removal phases: %+v", phases)
	}
	for index, phase := range phases {
		if phase.Action != []string{"validate", "verify-bindings", "apply"}[index] || phase.Request.ConfirmQuiesced != (phase.Action == "apply") {
			t.Fatalf("unexpected removal phase: %+v", phase)
		}
		data, _ := json.Marshal(phase.Request)
		decoded, err := Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("real backend rejected UI removal: %v", err)
		}
		if _, err := backend.Plan(home, decoded); err != nil {
			t.Fatal(err)
		}
		if phase.Action == "verify-bindings" {
			if err := backend.verifyBindings(ctx, home, decoded); err != nil {
				t.Fatal(err)
			}
		}
		if phase.Action == "apply" {
			if err := backend.Apply(ctx, home, decoded); err != nil {
				t.Fatal(err)
			}
		}
	}
	after, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil || !after.Catalog.Disabled || len(after.Catalog.Profiles) != 0 || after.Revision == before.Revision {
		t.Fatalf("removal did not persist: %+v %v", after, err)
	}
	reopened, err := backend.Discover(ctx, home)
	if err != nil || !reopened.Request.Catalog.Disabled || reopened.Request.ExpectedRevision != after.Revision {
		t.Fatalf("setup could not reopen removed catalog: %+v %v", reopened.Request, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "preserved application content" {
			t.Fatalf("application file changed: %s %v", path, err)
		}
	}
	backups, _ := filepath.Glob(filepath.Join(home, ".config/gpu-workload-supervisor/backups/activation-*/state.db"))
	if len(backups) != 1 {
		t.Fatalf("removal backup missing: %v", backups)
	}
	prior, err := ReadCatalog(ctx, backups[0])
	if err != nil || !reflect.DeepEqual(prior, before) {
		t.Fatalf("backup lost previous catalog: %+v %v", prior, err)
	}
	request.ExpectedRevision = after.Revision
	request.ConfirmQuiesced = true
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatalf("re-adding application failed: %v", err)
	}
}

func TestFinalRemovalRetainsActivationGuards(t *testing.T) {
	for _, failure := range []string{"confirmation", "stale-revision", "open-admission", "unfinished-work", "old-runtime-busy", "snapshot-over-stable"} {
		t.Run(failure, func(t *testing.T) {
			backend, home, request := fixture(t)
			ctx := context.Background()
			if err := backend.Apply(ctx, home, request); err != nil {
				t.Fatal(err)
			}
			before, err := ReadCatalog(ctx, request.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			request.ExpectedRevision = before.Revision
			request.Catalog = control.Catalog{Version: 1, Disabled: true}
			switch failure {
			case "confirmation":
				request.ConfirmQuiesced = false
			case "stale-revision":
				request.ExpectedRevision = "stale"
			case "open-admission", "unfinished-work":
				db, err := sql.Open("sqlite", request.Profile.StatePath)
				if err != nil {
					t.Fatal(err)
				}
				query := "UPDATE control_state SET admission='open'"
				if failure == "unfinished-work" {
					query = "INSERT INTO registered_work(request_id,lease_incarnation,lease_epoch,registered_at,workload) SELECT 'unfinished',lease_incarnation,lease_epoch,'2026-10-08T00:00:00Z','text' FROM control_state"
				}
				_, err = db.ExecContext(ctx, query)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "old-runtime-busy":
				backend.makeRuntime = func(r Request) (gpuruntime.Manager, error) {
					if !r.Catalog.Disabled {
						return idleRuntime{err: errors.New("former application is busy")}, nil
					}
					return idleRuntime{}, nil
				}
			case "snapshot-over-stable":
				profile := request.Profile
				profile.ActivatedRelease = "v0.1.8"
				acceptanceJSON(t, filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"), profile)
				if err := deployment.Write(request.Profile.StatePath, deployment.Marker{Version: 1, Release: "v0.1.8"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := backend.Apply(ctx, home, request); err == nil {
				t.Fatal("unsafe final removal accepted")
			}
			after, err := ReadCatalog(ctx, request.Profile.StatePath)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed catalog: %+v %v", after, err)
			}
		})
	}
}

func TestFinalOwnedRemovalUsesContentProofs(t *testing.T) {
	for _, modified := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "modified"}[modified], func(t *testing.T) {
			backend, home, request := fixture(t)
			backend.runCommand = fakeOwnedCommandFor(home)
			profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
			request.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
			ctx := context.Background()
			if err := backend.Apply(ctx, home, request); err != nil {
				t.Fatal(err)
			}
			before, err := ReadCatalog(ctx, request.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			request.ExpectedRevision = before.Revision
			request.Catalog = control.Catalog{Version: 2, Disabled: true}
			if modified {
				raw = []byte("foreign modified unit")
				acceptanceWrite(t, profile.NativeModel.LaunchFile, raw)
			}
			err = backend.Apply(ctx, home, request)
			if modified {
				if !errors.Is(err, ErrOwnedUnitModified) {
					t.Fatalf("modified owned removal = %v", err)
				}
				data, readErr := os.ReadFile(profile.NativeModel.LaunchFile)
				if readErr != nil || !bytes.Equal(data, raw) {
					t.Fatal("modified unit was removed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(profile.NativeModel.LaunchFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("proven owned unit retained: %v", err)
			}
			stateStore, err := store.Open(ctx, request.Profile.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			defer stateStore.Close()
			history, err := stateStore.CatalogAtRevision(ctx, before.Revision)
			if err != nil || !reflect.DeepEqual(history, before) {
				t.Fatalf("owned catalog audit history lost: %+v %v", history, err)
			}
		})
	}
}

func TestFinalRemovalResumesAfterCatalogCommit(t *testing.T) {
	backend, home, request := fixture(t)
	ctx := context.Background()
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatal(err)
	}
	before, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = before.Revision
	// Match the UI's explicit [] representation, not a nil Go slice.
	request.Catalog = control.Catalog{Version: 1, Disabled: true, Profiles: []control.WorkloadProfile{}}
	// Activation retains binaries after the catalog commit; failure there leaves
	// the maintenance fence and original request for a subsequent exact resume.
	binary := filepath.Join(backend.binaryDirectory, binaries[0])
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	if err := backend.Apply(ctx, home, request); err == nil {
		t.Fatal("missing postcommit binary was accepted")
	}
	committed, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil || !reflect.DeepEqual(committed.Catalog, request.Catalog) || committed.Revision == before.Revision {
		t.Fatalf("UI empty catalog changed at commit: %+v %v", committed, err)
	}
	marker, err := deployment.Read(request.Profile.StatePath)
	if err != nil || !marker.Maintenance {
		t.Fatalf("interrupted commit lost its fence: %+v %v", marker, err)
	}
	if err := os.WriteFile(binary, []byte("binary-"+binaries[0]), 0700); err != nil {
		t.Fatal(err)
	}
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatalf("exact removal resume failed: %v", err)
	}
	request.ExpectedRevision = committed.Revision
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatalf("disabled catalog reapply failed: %v", err)
	}
	after, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil || !reflect.DeepEqual(after, committed) {
		t.Fatalf("resume/reapply advanced committed revision: %+v %v", after, err)
	}
	if err := deployment.Check(request.Profile.StatePath, deployment.Release); err != nil {
		t.Fatalf("resume left maintenance fence: %v", err)
	}
}
