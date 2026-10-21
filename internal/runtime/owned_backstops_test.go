package runtime

import (
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

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
