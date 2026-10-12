package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

func TestDiscoveryIgnoresApplicationHelperNames(t *testing.T) {
	for _, tc := range []struct {
		unit, metadata string
		err            error
	}{
		{"comfyui-updater.service", "ExecStart={ path=/usr/bin/python3 ; argv[]=/usr/bin/python3 /opt/ComfyUI/update.py ; }\n", nil},
		{"ollama-update.service", "ExecStart={ path=/usr/bin/true ; argv[]=/usr/bin/true ; }\n", nil},
		{"llama-model-helper.service", "ExecStart={ path=/bin/sh ; argv[]=/bin/sh -c llama-server ; }\n", nil},
		{"vllm-helper.service", "ExecStart={ path=/usr/bin/true ; argv[]=/usr/bin/true ; }\nExecStartPre=unparseable metadata\n", nil},
		{"comfyui-updater.service", "", errors.New("user bus unavailable")},
	} {
		t.Run(tc.unit+tc.metadata, func(t *testing.T) {
			b := Backend{runCommand: func(context.Context, string, ...string) ([]byte, error) {
				return []byte(tc.metadata), tc.err
			}, inspectAutomatic: func(string, string) (gpuruntime.AutomaticLaunch, error) {
				t.Fatal("helper source was inspected as an application")
				return gpuruntime.AutomaticLaunch{}, nil
			}}
			result := Discovery{}
			b.discoverUnit(context.Background(), &result, tc.unit)
			if len(result.Applications) != 0 || len(result.Units) != 0 || len(result.Errors) != 0 {
				t.Fatalf("helper became an application candidate: %+v", result)
			}
		})
	}
}

func TestDiscoveryRetainsCatalogApplicationIdentityWhenInspectionFails(t *testing.T) {
	for _, tc := range []struct {
		app, status string
		comfy       bool
		err         error
	}{
		{"comfyui", "unsupported", true, nil},
		{"ollama", inspectionFailedStatus, false, errors.New("user bus unavailable")},
	} {
		t.Run(tc.app, func(t *testing.T) {
			p := control.WorkloadProfile{Unit: "custom-worker.service"}
			if tc.comfy {
				p.LaunchBinding = &control.LaunchBinding{Runtime: tc.app}
			} else {
				p.NativeModel = &control.NativeModel{Runtime: tc.app}
			}
			result := Discovery{Request: Request{Catalog: control.Catalog{Profiles: []control.WorkloadProfile{p}}}}
			b := Backend{runCommand: func(context.Context, string, ...string) ([]byte, error) {
				return []byte("ExecStart={ path=/usr/bin/true ; argv[]=/usr/bin/true ; }\n"), tc.err
			}}
			b.discoverUnit(context.Background(), &result, p.Unit)
			if len(result.Applications) != 1 {
				t.Fatalf("saved application was lost: %+v", result)
			}
			found := result.Applications[0]
			if found.App != tc.app || found.Unit != p.Unit || found.ConfigurationStatus != tc.status || found.Recognized || found.Binding != nil {
				t.Fatalf("failed saved binding became executable evidence: %+v", found)
			}
		})
	}
}

func TestStoppedNativeApplicationsRemainInstalledWithoutEndpointProbes(t *testing.T) {
	for _, app := range []string{"ollama", "vllm"} {
		t.Run(app, func(t *testing.T) {
			b, draft, _ := automaticFixture(t, app)
			b.probeApplication = func(context.Context, ProbeRequest) (ApplicationCandidate, error) {
				t.Fatal("stopped installation endpoint was probed")
				return ApplicationCandidate{}, nil
			}
			result := Discovery{}
			b.discoverUnit(context.Background(), &result, draft.Binding.Unit)
			if len(result.Applications) != 1 {
				t.Fatalf("stopped installation was lost: %+v", result)
			}
			found := result.Applications[0]
			if !found.Recognized || found.Binding == nil || found.InstanceStatus != "not-running" || found.InventoryStatus != "unknown" {
				t.Fatalf("stopped installation became missing or unreachable: %+v", found)
			}
		})
	}
}

func TestUnreachableNativeAddressDoesNotEstablishMissingExecutable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()
	for _, app := range []string{"ollama", "vllm"} {
		t.Run(app, func(t *testing.T) {
			found, err := Probe(context.Background(), ProbeRequest{App: app, Endpoint: endpoint})
			if err != nil || found.InstanceStatus != "unreachable" || found.Recognized || found.Binding != nil || found.Reference != "" {
				t.Fatalf("address failure became installation evidence: %+v %v", found, err)
			}
			missing, err := Probe(context.Background(), ProbeRequest{App: app, Reference: filepath.Join(t.TempDir(), app), ReferenceKind: "application"})
			if err != nil || missing.InstanceStatus != "missing" || missing.Recognized || missing.Binding != nil {
				t.Fatalf("missing executable became an address failure: %+v %v", missing, err)
			}
		})
	}
}

func TestDiscoveryRetainsHistoricalComfyAdapterWithoutInferringSystemdRuntime(t *testing.T) {
	for _, tc := range []struct{ adapter, unit, app string }{{"comfyui", "custom-worker.service", "comfyui"}, {"systemd", "comfyui-worker.service", ""}} {
		t.Run(tc.adapter, func(t *testing.T) {
			p := control.WorkloadProfile{Adapter: tc.adapter, Unit: tc.unit}
			result := Discovery{Request: Request{Catalog: control.Catalog{Profiles: []control.WorkloadProfile{p}}}}
			b := Backend{runCommand: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("user bus unavailable")
			}}
			b.discoverUnit(context.Background(), &result, p.Unit)
			if len(result.Applications) != 1 {
				t.Fatalf("historical application was lost: %+v", result)
			}
			found := result.Applications[0]
			if found.App != tc.app || found.ConfigurationStatus != inspectionFailedStatus || found.Recognized || found.Binding != nil {
				t.Fatalf("historical adapter became launch evidence: %+v", found)
			}
		})
	}
}

func TestDiscoveryRetainsConfiguredGenericUnitWithoutApplicationEvidence(t *testing.T) {
	for _, tc := range []struct {
		status string
		err    error
	}{
		{"unsupported", nil},
		{inspectionFailedStatus, errors.New("user bus unavailable")},
	} {
		t.Run(tc.status, func(t *testing.T) {
			p := control.WorkloadProfile{ID: "ci-workload", Label: "CI workload", Adapter: "systemd", Unit: "gws-ci-workload.service"}
			result := Discovery{Request: Request{Catalog: control.Catalog{Profiles: []control.WorkloadProfile{p}}}}
			b := Backend{runCommand: func(context.Context, string, ...string) ([]byte, error) {
				return []byte("ExecStart={ path=/usr/bin/sleep ; argv[]=/usr/bin/sleep infinity ; }\n"), tc.err
			}, inspectAutomatic: func(string, string) (gpuruntime.AutomaticLaunch, error) {
				t.Fatal("generic configured source was inspected as an application")
				return gpuruntime.AutomaticLaunch{}, nil
			}}
			b.discoverUnit(context.Background(), &result, p.Unit)
			if len(result.Applications) != 1 {
				t.Fatalf("configured workload diagnostic was lost: %+v", result)
			}
			found := result.Applications[0]
			if found.Unit != p.Unit || found.App != "" || found.ConfigurationStatus != tc.status || found.Recognized || found.Binding != nil || len(found.Models) != 0 {
				t.Fatalf("generic workload acquired application evidence: %+v", found)
			}
			if len(result.Units) != 0 || result.Request.Catalog.Profiles[0] != p {
				t.Fatalf("generic workload changed discovery or catalog bindings: %+v", result)
			}
		})
	}
}
