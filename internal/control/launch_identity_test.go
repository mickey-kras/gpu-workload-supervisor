package control

import "testing"

func TestSharedOwnedLaunchRequiresSameExecutableAndGPUIdentity(t *testing.T) {
	first := ownedOllamaProfile("first", "model-a")
	second := ownedOllamaProfile("second", "model-b")
	for _, field := range []string{"gpuUUID", "executable"} {
		t.Run(field, func(t *testing.T) {
			a, b := first, second
			an, bn := *first.NativeModel, *second.NativeModel
			a.NativeModel = &an
			b.NativeModel = &bn
			ao, bo := *an.Owned, *bn.Owned
			an.Owned = &ao
			bn.Owned = &bo
			if field == "gpuUUID" {
				an.GPUUUID = "GPU-01234567-89ab-cdef-0123-456789abcdef"
			} else {
				ao.Executable = "/opt/trusted/ollama"
			}
			if err := (Catalog{Version: 2, Profiles: []WorkloadProfile{a, b}}).Validate(); err == nil {
				t.Fatal("shared launch allowed conflicting", field)
			}
		})
	}
}
