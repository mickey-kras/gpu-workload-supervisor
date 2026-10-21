package runtime

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestLaunchInspectionRejectsUninspectedOperationalDirectives(t *testing.T) {
	good := []byte("[Service]\nEnvironment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=localhost:9000\nEnvironment=CUDA_VISIBLE_DEVICES=GPU-01234567-89ab-cdef-0123-456789abcdef\nExecStart=/usr/bin/ollama serve\n")
	n := control.NativeModel{Runtime: "ollama", Endpoint: "http://localhost:9000", GPUUUID: "GPU-01234567-89ab-cdef-0123-456789abcdef"}
	validate := func(string) error { return nil }
	if _, err := inspectAutomaticLaunch(good, n.Runtime, validate); err != nil {
		t.Fatal("baseline launch must qualify", err)
	}
	directives := []string{
		"EnvironmentFile=/etc/runtime-environment",
		"EnvironmentFile=-/etc/optional-runtime-environment",
		"UnsetEnvironment=CUDA_VISIBLE_DEVICES OLLAMA_HOST OLLAMA_NO_CLOUD",
		"PassEnvironment=LD_PRELOAD",
		"PAMName=login",
		"Sockets=unbound.socket",
		"FailureAction=exit",
		"StartLimitAction=exit-force",
		"ExecCondition=/usr/bin/perl /tmp/unbound.pl",
		"ExecStartPost=/usr/bin/perl /tmp/unbound.pl",
		"ExecStop=/usr/bin/perl /tmp/unbound.pl",
		"ExecStopPost=/usr/bin/perl /tmp/unbound.pl",
		"ExecReload=/usr/bin/perl /tmp/unbound.pl",
		"RootDirectory=/srv/alternate-root",
		"RootImage=/srv/alternate-root.img",
		"BindPaths=/tmp/ollama:/usr/bin/ollama",
		"BindReadOnlyPaths=/tmp/ollama:/usr/bin/ollama",
		"TemporaryFileSystem=/usr",
		"MountImages=/srv/alternate-root.img:/usr",
		"ExtensionImages=/srv/alternate-usr.img",
		"ExtensionDirectories=/srv/alternate-usr",
	}
	for _, directive := range directives {
		t.Run(strings.SplitN(directive, "=", 2)[0]+"/"+strings.SplitN(directive, "=", 2)[1], func(t *testing.T) {
			raw := append(append([]byte(nil), good...), []byte(directive+"\n")...)
			if _, err := inspectAutomaticLaunch(raw, n.Runtime, validate); !errors.Is(err, ErrLaunchUnsupported) {
				t.Errorf("strict inspection accepted operational directive: %v", err)
			}
			for _, inDropIn := range []bool{false, true} {
				dir := t.TempDir()
				path := filepath.Join(dir, "worker.service")
				primary := raw
				var dropPaths []string
				binding := n
				if inDropIn {
					primary = good
					drop := filepath.Join(dir, "10-operational.conf")
					content := []byte("[Service]\n" + directive + "\n")
					if err := os.WriteFile(drop, content, 0600); err != nil {
						t.Fatal(err)
					}
					dropPaths = []string{drop}
					binding.DropIns = []control.LaunchSource{{Path: drop, SHA256: fmt.Sprintf("%x", sha256.Sum256(content))}}
				}
				if err := os.WriteFile(path, primary, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := inspectAutomaticLaunchSourcesWithValidator(path, n.Runtime, dropPaths, validate); !errors.Is(err, ErrLaunchUnsupported) {
					t.Errorf("automatic inspection accepted directive (drop-in=%v): %v", inDropIn, err)
				}
				if _, err := inspectQualifiedNativeLaunch(path, binding, validate); !errors.Is(err, ErrLaunchUnsupported) {
					t.Errorf("binding inspection accepted directive (drop-in=%v): %v", inDropIn, err)
				}
				for _, source := range binding.DropIns {
					after, err := os.ReadFile(source.Path)
					if err != nil || fmt.Sprintf("%x", sha256.Sum256(after)) != source.SHA256 {
						t.Fatal("inspection modified the adopted drop-in", err)
					}
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, primary) {
					t.Fatal("inspection modified the adopted unit", err)
				}
			}
		})
	}
}

func TestLaunchInspectionRetainsHarmlessUnknownSettings(t *testing.T) {
	raw := []byte("[Unit]\nDocumentation=https://example.org/runtime\n[Service]\nEnvironment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=localhost:9000\nExecStart=/usr/bin/ollama serve\nSyslogIdentifier=local-model\n")
	validate := func(string) error { return nil }
	if _, err := inspectAutomaticLaunch(raw, "ollama", validate); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.service")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(path, "ollama", nil, validate); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchInspectionRejectsUnboundUnitEffects(t *testing.T) {
	good := []byte("[Service]\nEnvironment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=localhost:9000\nExecStart=/usr/bin/ollama serve\n")
	validate := func(string) error { return nil }
	for _, key := range []string{
		"Requires", "Requisite", "BindsTo", "PartOf", "Upholds", "Conflicts", "OnFailure", "OnSuccess",
		"PropagatesReloadTo", "ReloadPropagatedFrom", "PropagatesStopTo", "StopPropagatedFrom", "JoinsNamespaceOf",
		"RequiresMountsFor", "WantsMountsFor", "Wants",
		"FailureAction", "SuccessAction", "StartLimitAction", "JobTimeoutAction",
	} {
		t.Run(key, func(t *testing.T) {
			value := "unbound-gpu.service"
			if strings.HasSuffix(key, "Action") {
				value = "exit-force"
			}
			if strings.HasSuffix(key, "MountsFor") {
				value = "/srv/unbound-models"
			}
			dependency := []byte("[Unit]\n" + key + "=" + value + "\n")
			raw := append(append([]byte(nil), dependency...), good...)
			if _, err := inspectAutomaticLaunch(raw, "ollama", validate); !errors.Is(err, ErrLaunchUnsupported) {
				t.Errorf("strict inspection accepted unit effect: %v", err)
			}
			for _, inDropIn := range []bool{false, true} {
				dir := t.TempDir()
				path := filepath.Join(dir, "worker.service")
				primary := raw
				var dropPaths []string
				binding := control.NativeModel{Runtime: "ollama", Endpoint: "http://localhost:9000"}
				if inDropIn {
					primary = good
					drop := filepath.Join(dir, "10-dependency.conf")
					if err := os.WriteFile(drop, dependency, 0600); err != nil {
						t.Fatal(err)
					}
					dropPaths = []string{drop}
					binding.DropIns = []control.LaunchSource{{Path: drop, SHA256: fmt.Sprintf("%x", sha256.Sum256(dependency))}}
				}
				if err := os.WriteFile(path, primary, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := inspectAutomaticLaunchSourcesWithValidator(path, "ollama", dropPaths, validate); !errors.Is(err, ErrLaunchUnsupported) {
					t.Errorf("automatic inspection accepted unit effect (drop-in=%v): %v", inDropIn, err)
				}
				if _, err := inspectQualifiedNativeLaunch(path, binding, validate); !errors.Is(err, ErrLaunchUnsupported) {
					t.Errorf("binding inspection accepted unit effect (drop-in=%v): %v", inDropIn, err)
				}
				for _, source := range binding.DropIns {
					after, err := os.ReadFile(source.Path)
					if err != nil || fmt.Sprintf("%x", sha256.Sum256(after)) != source.SHA256 {
						t.Fatal("inspection modified the adopted drop-in", err)
					}
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, primary) {
					t.Fatal("inspection modified the adopted unit", err)
				}
			}
		})
	}
}

func TestLaunchInspectionPreservesQualifiedNetworkDependencyAndOrdering(t *testing.T) {
	raw := []byte("[Unit]\nDescription=Local model\nWants=network-online.target\nAfter=network-online.target\nBefore=shutdown.target\n[Service]\nEnvironment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=localhost:9000\nExecStart=/usr/bin/ollama serve\n")
	validate := func(string) error { return nil }
	if _, err := inspectAutomaticLaunch(raw, "ollama", validate); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.service")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(path, "ollama", nil, validate); err != nil {
		t.Fatal(err)
	}
}
