package setup

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

type trainerInspector struct {
	gpuruntime.Manager
	inspect func(context.Context, control.Workload) error
}

func (i trainerInspector) InspectTrainer(ctx context.Context, id control.Workload) error {
	return i.inspect(ctx, id)
}

func trainerInspectionRequest(t *testing.T) (Backend, Request) {
	t.Helper()
	b, _, request := fixture(t)
	request.Catalog.Version = 4
	request.Catalog.Profiles = []control.WorkloadProfile{{
		ID: "trainer", Label: "Trainer", Adapter: "systemd", Unit: "trainer.service",
		Cgroup: "/user.slice/trainer.service", BootPolicy: "stop-to-idle", RequiredMiB: 128,
		Trainer: &control.TrainerBinding{
			Version: 1, GPUUUID: "GPU-11111111-1111-1111-1111-111111111111",
			Executable: "/trusted/adapter", ExecutableSHA256: strings.Repeat("a", 64),
			RecipeFile: "/private/recipe.json", RecipeSHA256: strings.Repeat("b", 64),
			LaunchFile: "/private/trainer.service", LaunchSHA256: strings.Repeat("c", 64),
			JobFile: "/private/job.json", TerminalFile: "/private/terminal.json",
		},
	}}
	return b, request
}

func TestTrainerInspectionRequiresExplicitValidBinding(t *testing.T) {
	if _, err := SystemBackend().InspectTrainer(context.Background(), Request{}, "trainer"); err == nil {
		t.Fatal("public inspection accepted an empty request")
	}
	for _, mutation := range []func(*Request){
		func(r *Request) { r.Version = 0 },
		func(r *Request) { r.Catalog.Profiles[0].RequiredMiB = 0 },
		func(r *Request) { r.Catalog.Profiles[0].Trainer.GPUUUID = "0" },
		func(r *Request) { r.Catalog.Profiles[0].Trainer.ExecutableSHA256 = "" },
	} {
		b, request := trainerInspectionRequest(t)
		mutation(&request)
		b.makeRuntime = func(Request) (gpuruntime.Manager, error) {
			t.Fatal("invalid candidate reached runtime")
			return nil, nil
		}
		found, err := b.InspectTrainer(context.Background(), request, "trainer")
		if err == nil || found.BindingStatus == "verified" || found.QualificationStatus != "unqualified" {
			t.Fatalf("invalid inspection: %+v, %v", found, err)
		}
	}
	b, _, request := fixture(t)
	if _, err := b.InspectTrainer(context.Background(), request, "text"); err == nil {
		t.Fatal("inference profile inspected as trainer")
	}
	if _, err := b.InspectTrainer(context.Background(), request, "unknown"); err == nil {
		t.Fatal("unknown profile inspected")
	}
}

func TestTrainerInspectionCannotVerifyCancelledObservation(t *testing.T) {
	b, request := trainerInspectionRequest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.makeRuntime = func(Request) (gpuruntime.Manager, error) {
		t.Fatal("cancelled inspection reached runtime")
		return nil, nil
	}
	if found, err := b.InspectTrainer(ctx, request, "trainer"); err == nil || found.BindingStatus == "verified" {
		t.Fatalf("cancelled inspection accepted: %+v %v", found, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	b.makeRuntime = func(Request) (gpuruntime.Manager, error) {
		return trainerInspector{inspect: func(context.Context, control.Workload) error {
			cancel()
			return nil
		}}, nil
	}
	if found, err := b.InspectTrainer(ctx, request, "trainer"); err == nil || found.BindingStatus == "verified" {
		t.Fatalf("cancelled observation accepted: %+v %v", found, err)
	}
}

func TestTrainerDiscoveryReportsBindingEvidenceWithoutApprovingRuntime(t *testing.T) {
	for _, failure := range []error{nil, errors.New("private-path-secret")} {
		b, request := trainerInspectionRequest(t)
		original := request.Catalog.Clone()
		calls := 0
		b.makeRuntime = func(Request) (gpuruntime.Manager, error) {
			return trainerInspector{inspect: func(ctx context.Context, id control.Workload) error {
				calls++
				if id != "trainer" {
					t.Fatal(id)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("inspection has no deadline")
				}
				return failure
			}}, nil
		}
		result := Discovery{Request: request}
		b.discoverTrainers(context.Background(), &result)
		if calls != 1 || len(result.Trainers) != 1 || len(result.Applications) != 0 || !reflect.DeepEqual(original, request.Catalog) {
			t.Fatalf("unexpected discovery side effects: %+v", result)
		}
		found := result.Trainers[0]
		if found.QualificationStatus != "unqualified" || (found.BindingStatus == "verified") != (failure == nil) || strings.Contains(found.NextStep, "private-path-secret") {
			t.Fatalf("invalid qualification result: %+v", found)
		}
	}
}

func TestTrainerInspectionReportsUnavailableRuntime(t *testing.T) {
	b, request := trainerInspectionRequest(t)
	for _, makeRuntime := range []func(Request) (gpuruntime.Manager, error){
		func(Request) (gpuruntime.Manager, error) { return nil, errors.New("private-path-secret") },
		func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil },
	} {
		b.makeRuntime = makeRuntime
		found, err := b.InspectTrainer(context.Background(), request, "trainer")
		if err == nil || found.BindingStatus != inspectionFailedStatus || strings.Contains(err.Error(), "private-path-secret") {
			t.Fatalf("unexpected inspection result: %+v %v", found, err)
		}
	}
	result := Discovery{Request: Request{Catalog: control.Catalog{Profiles: []control.WorkloadProfile{{ID: "text"}}}}}
	b.discoverTrainers(context.Background(), &result)
	if result.Trainers == nil || len(result.Trainers) != 0 {
		t.Fatal("empty trainer discovery must be an empty list")
	}
}

func TestDiscoveryPreservesTrainerCatalogWhileAddingInspection(t *testing.T) {
	b, request := trainerInspectionRequest(t)
	_, home, _ := fixture(t)
	if err := b.Apply(context.Background(), home, request); err != nil {
		t.Fatal(err)
	}
	b.makeRuntime = func(Request) (gpuruntime.Manager, error) {
		return trainerInspector{inspect: func(context.Context, control.Workload) error { return nil }}, nil
	}
	b.probeApplication = func(_ context.Context, r ProbeRequest) (ApplicationCandidate, error) { return candidate(r), nil }
	found, err := b.Discover(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(found.Request.Catalog, request.Catalog) || len(found.Trainers) != 1 || found.Trainers[0].BindingStatus != "verified" || found.Trainers[0].QualificationStatus != "unqualified" {
		t.Fatalf("trainer catalog or evidence changed: %+v", found)
	}
}
