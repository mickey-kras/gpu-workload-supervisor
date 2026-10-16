package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func contractTrainerCatalog() Catalog {
	return Catalog{Version: 4, Profiles: []WorkloadProfile{{ID: "trainer", Label: "Trainer", Adapter: "systemd", Unit: "trainer.service", Cgroup: "/training/trainer.service", RequiredMiB: 1024, BootPolicy: "stop-to-idle", Trainer: &TrainerBinding{Version: 1, GPUUUID: "GPU-11111111-1111-1111-1111-111111111111", LaunchFile: "/opt/trainer/unit.service", LaunchSHA256: strings.Repeat("a", 64), Executable: "/opt/trainer/train", ExecutableSHA256: strings.Repeat("b", 64), RecipeFile: "/opt/trainer/recipe.json", RecipeSHA256: strings.Repeat("c", 64), JobFile: "/opt/trainer/job.json", TerminalFile: "/opt/trainer/terminal.json"}}}}
}

func TestTrainingContractCatalogStrict(t *testing.T) {
	c := contractTrainerCatalog()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*Catalog){
		"systemd-version": func(c *Catalog) { c.Profiles[0].SystemdVersion = 255 },
		"old-version":     func(c *Catalog) { c.Version = 2 },
		"retain":          func(c *Catalog) { c.Profiles[0].BootPolicy = "retain" },
		"health-endpoint": func(c *Catalog) { c.Profiles[0].HealthURL = "http://localhost:8080" },
		"no-budget":       func(c *Catalog) { c.Profiles[0].RequiredMiB = 0 },
		"gpu-index":       func(c *Catalog) { c.Profiles[0].Trainer.GPUUUID = "0" },
		"relative-path":   func(c *Catalog) { c.Profiles[0].Trainer.Executable = "bin/train" },
		"path-expansion":  func(c *Catalog) { c.Profiles[0].Trainer.Executable = "/$HOME/train" },
		"duplicate-path":  func(c *Catalog) { c.Profiles[0].Trainer.JobFile = c.Profiles[0].Trainer.TerminalFile },
		"bad-digest":      func(c *Catalog) { c.Profiles[0].Trainer.RecipeSHA256 = strings.Repeat("z", 64) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := c.Clone()
			mutate(&candidate)
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = DecodeCatalogBytes(raw); err == nil {
				t.Fatal("invalid training catalog accepted")
			}
		})
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"unknown": strings.Replace(string(raw), `"trainer":{`, `"trainer":{"unexpected":true,`, 1), "duplicate": strings.Replace(string(raw), `"trainer":{`, `"trainer":{"version":1,`, 1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCatalogBytes([]byte(raw)); err == nil {
				t.Fatal("non-strict trainer JSON accepted")
			}
		})
	}
	clone := c.Clone()
	clone.Profiles[0].Trainer.RecipeFile = "/changed"
	if c.Profiles[0].Trainer.RecipeFile == "/changed" {
		t.Fatal("clone shares trainer binding")
	}
}

func TestTrainingContractPreservesLegacyCatalog(t *testing.T) {
	for _, version := range []int{1, 2} {
		c := Catalog{Version: version, Profiles: []WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/text", HealthURL: "http://localhost:8080/health"}}}
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeCatalogBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != string(raw) {
			t.Fatalf("legacy catalog rewritten: %s", encoded)
		}
	}
}

func TestTrainingContractTerminalIdentity(t *testing.T) {
	job := TrainingJob{ID: "job", Token: "token", Fence: Fence{Incarnation: "incarnation", Epoch: 2}, RecipeSHA256: strings.Repeat("a", 64), InvocationID: strings.Repeat("b", 32)}
	terminal := TrainingTerminal{Version: 1, ID: job.ID, Token: job.Token, Fence: job.Fence, RecipeSHA256: job.RecipeSHA256, InvocationID: job.InvocationID, Outcome: "completed"}
	if !terminal.Matches(job) {
		t.Fatal("valid terminal rejected")
	}
	for name, mutate := range map[string]func(*TrainingTerminal){"version": func(e *TrainingTerminal) { e.Version++ }, "id": func(e *TrainingTerminal) { e.ID = "other" }, "token": func(e *TrainingTerminal) { e.Token = "other" }, "epoch": func(e *TrainingTerminal) { e.Fence.Epoch-- }, "incarnation": func(e *TrainingTerminal) { e.Fence.Incarnation = "old" }, "recipe": func(e *TrainingTerminal) { e.RecipeSHA256 = "other" }, "invocation": func(e *TrainingTerminal) { e.InvocationID = "other" }, "outcome": func(e *TrainingTerminal) { e.Outcome = "running" }} {
		t.Run(name, func(t *testing.T) {
			candidate := terminal
			mutate(&candidate)
			if candidate.Matches(job) {
				t.Fatal("stale or invalid terminal accepted")
			}
		})
	}
	job.InvocationID = ""
	terminal.InvocationID = ""
	if terminal.Matches(job) {
		t.Fatal("terminal accepted before invocation recorded")
	}
}

func TestTrainingContractProtectsOtherLaunchInputs(t *testing.T) {
	c := contractTrainerCatalog()
	app := WorkloadProfile{ID: "images", Label: "Images", Adapter: "systemd", Unit: "images.service", Cgroup: "/images.service", HealthURL: "http://127.0.0.1:8188/system_stats", LaunchBinding: &LaunchBinding{Runtime: "comfyui", Endpoint: "http://127.0.0.1:8188", LaunchFile: "/opt/images.service", LaunchSHA256: strings.Repeat("d", 64), DropIns: []LaunchSource{{Path: "/opt/images.service.d/environment.conf", SHA256: strings.Repeat("e", 64)}}}}
	c.Profiles = append(c.Profiles, app)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{app.LaunchBinding.LaunchFile, app.LaunchBinding.DropIns[0].Path} {
		for _, reverse := range []bool{false, true} {
			candidate := c.Clone()
			candidate.Profiles[0].Trainer.JobFile = file
			if reverse {
				candidate.Profiles[0], candidate.Profiles[1] = candidate.Profiles[1], candidate.Profiles[0]
			}
			if err := candidate.Validate(); err == nil {
				t.Fatal("trainer output can overwrite accepted launch source")
			}
		}
	}
}
