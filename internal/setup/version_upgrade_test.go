package setup

import (
	"context"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

func TestNewerRequiresHigherStableRelease(t *testing.T) {
	for _, tc := range []struct {
		name, next, previous string
		want                 bool
	}{
		{"snapshot to higher stable", "0.1.8", "0.1.7-SNAPSHOT-5f16eb4", true},
		{"prefixed snapshot", "v0.1.8", "v0.1.7-SNAPSHOT-5f16eb4", true},
		{"prerelease to higher stable", "1.3.0", "1.2.0-rc.1", true},
		{"stable patch", "1.2.4", "1.2.3", true},
		{"stable minor", "1.3.0", "1.2.9", true},
		{"stable major", "2.0.0", "1.9.9", true},
		{"same release", "1.2.3", "1.2.3", false},
		{"downgrade", "0.1.6", "0.1.7-SNAPSHOT-5f16eb4", false},
		{"same snapshot core", "0.1.7", "0.1.7-SNAPSHOT-5f16eb4", false},
		{"snapshot hash is not chronology", "0.1.7-SNAPSHOT-fff", "0.1.7-SNAPSHOT-aaa", false},
		{"higher snapshot target", "0.1.8-SNAPSHOT-fff", "0.1.7-SNAPSHOT-aaa", false},
		{"stable to prerelease", "0.1.8-rc.1", "0.1.7", false},
		{"build metadata has no order", "1.2.3+new", "1.2.3+old", false},
		{"higher stable with metadata", "1.2.4+build.1", "1.2.3+build.2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newer(tc.next, tc.previous); got != tc.want {
				t.Fatalf("newer(%q, %q) = %v, want %v", tc.next, tc.previous, got, tc.want)
			}
		})
	}
	for _, invalid := range []string{"", "dev", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.03", "1.+2.3", "1.-2.3", "1.2.3-", "1.2.3-01", "1.2.3+", "1.2.3-SNAPSHOT_bad", " 1.2.3", "1.2.3\n", "vv1.2.3"} {
		t.Run("invalid/"+invalid, func(t *testing.T) {
			if newer("9.9.9", invalid) || newer(invalid, "0.0.0") {
				t.Fatalf("invalid version %q accepted", invalid)
			}
		})
	}
}

func TestApplySnapshotReapplyAndHigherStableUpgrade(t *testing.T) {
	backend, home, request := fixture(t)
	ctx := context.Background()
	prior := deployment.Release
	t.Cleanup(func() { deployment.Release = prior })
	deployment.Release = "0.1.7-SNAPSHOT-5f16eb4"
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatal(err)
	}
	catalog, err := ReadCatalog(ctx, request.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = catalog.Revision
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatal("same snapshot reapply rejected:", err)
	}
	for _, rejected := range []string{"0.1.7", "0.1.6", "0.1.8-SNAPSHOT-ffffff", "0.1.8-", "dev"} {
		deployment.Release = rejected
		if err := backend.Apply(ctx, home, request); err == nil {
			t.Fatalf("unsafe target %q accepted", rejected)
		}
		marker, err := deployment.Read(request.Profile.StatePath)
		if err != nil || marker.Release != "0.1.7-SNAPSHOT-5f16eb4" || marker.Maintenance {
			t.Fatalf("rejected upgrade changed activation: %+v %v", marker, err)
		}
	}
	deployment.Release = "0.1.8"
	if err := backend.Apply(ctx, home, request); err != nil {
		t.Fatal("snapshot to higher stable upgrade rejected:", err)
	}
	if err := deployment.Check(request.Profile.StatePath, "0.1.8"); err != nil {
		t.Fatal(err)
	}
	found, err := backend.Discover(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if found.Request.Profile.ActivatedRelease != "0.1.8" || found.Request.ExpectedRevision != catalog.Revision {
		t.Fatalf("upgrade did not preserve catalog and activate stable profile: %+v", found)
	}
}
