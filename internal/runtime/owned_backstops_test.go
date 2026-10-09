package runtime

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestOwnedBackstopRenderAndQualification(t *testing.T) {
	for _, runtimeName := range []string{"ollama", "llama.cpp", "vllm"} {
		t.Run(runtimeName, func(t *testing.T) {
			modelPath := filepath.Join(t.TempDir(), "model")
			if runtimeName == "vllm" {
				if err := os.Mkdir(modelPath, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(modelPath, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p := ownedRenderProfile(runtimeName, "middle", &control.OwnedLaunch{Port: 9100, ModelPath: modelPath, Alias: "selected"})
			if runtimeName == "ollama" {
				p.NativeModel.Owned.ModelPath = ""
				p.NativeModel.Owned.Alias = ""
			}
			p.NativeModel.Owned.Conflicts = "gws-owned-a.service gws-owned-z.service"
			raw, err := RenderOwnedUnit(p)
			if err != nil {
				t.Fatal(err)
			}
			want := "Conflicts=gws-owned-a.service gws-owned-z.service\nAfter=gws-owned-a.service\n"
			if !strings.Contains(string(raw), want) || !strings.Contains(string(raw), "Restart=no\n") || strings.Contains(string(raw), "StartLimit") {
				t.Fatalf("wrong backstops: %s", raw)
			}
			p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
			if err := verifyOwnedSpecWithValidator(p, func(string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			adopted := *p.NativeModel
			adopted.Owned = nil
			if err := qualifyNativeLaunchWithValidator(raw, adopted, func(string) error { return nil }); err == nil {
				t.Fatal("dependency directives broadened adopted grammar")
			}
			for _, changed := range []string{
				strings.Replace(string(raw), "Conflicts=", "Conflicts=foreign.service ", 1),
				strings.Replace(string(raw), "After=gws-owned-a.service", "After=gws-owned-z.service", 1),
				strings.Replace(string(raw), "After=gws-owned-a.service", "After=gws-owned-a.service\nAfter=gws-owned-a.service", 1),
				strings.Replace(string(raw), "Restart=no", "StartLimitBurst=1\nRestart=no", 1),
			} {
				if err := qualifyNativeLaunchWithValidator([]byte(changed), *p.NativeModel, func(string) error { return nil }); err == nil {
					t.Fatal("unsupported owned dependency accepted", changed)
				}
			}
		})
	}
}

func TestOwnedBackstopOrderingAndRejectsInvalidPeers(t *testing.T) {
	for _, id := range []string{"a", "m", "z"} {
		p := ownedRenderProfile("llama.cpp", id, &control.OwnedLaunch{Port: 9100, ModelPath: "/models/m"})
		peers := []string{}
		for _, other := range []string{"a", "m", "z"} {
			if other != id {
				peers = append(peers, "gws-owned-"+other+".service")
			}
		}
		p.NativeModel.Owned.Conflicts = strings.Join(peers, " ")
		raw, err := RenderOwnedUnit(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if after, ok := strings.CutPrefix(line, "After="); ok {
				for _, peer := range strings.Fields(after) {
					if peer >= p.Unit {
						t.Fatal("ordering cycle possible", p.Unit, peer)
					}
				}
			}
		}
	}
	p := ownedRenderProfile("llama.cpp", "m", &control.OwnedLaunch{Port: 9100, ModelPath: "/models/m"})
	for _, peers := range []string{"foreign.service", p.Unit, "gws-owned-z.service gws-owned-a.service", "gws-owned-a.service gws-owned-a.service", "gws-owned-a.service\nAfter=external.service", "gws-owned-%n.service", "gws-owned-a.service  gws-owned-z.service", strings.Repeat("gws-owned-a.service ", 33)} {
		p.NativeModel.Owned.Conflicts = peers
		if _, err := RenderOwnedUnit(p); err == nil {
			t.Fatal("unsafe peers rendered", peers)
		}
	}
}
