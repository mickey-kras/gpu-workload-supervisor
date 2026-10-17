package llmtraining

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func jobFixture(t *testing.T) (Recipe, jobEnvelope, string, string) {
	t.Helper()
	r := fixture(t)
	root := filepath.Dir(r.OutputDirectory)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	recipePath := filepath.Join(root, "recipe.json")
	digest := writeFixture(t, recipePath, string(raw))
	e := jobEnvelope{Version: 1, RecipeFile: recipePath, TerminalFile: filepath.Join(root, "terminal.json"), Job: control.TrainingJob{ID: "private job", Token: "completion-credential-never-published", Workload: "trainer", Fence: control.Fence{Incarnation: "supervisor", Epoch: 2}, CatalogRevision: "catalog-revision", RecipeSHA256: digest, Status: "launching", UpdatedAt: time.Now().UTC()}}
	jobPath, planPath := filepath.Join(root, "job.json"), filepath.Join(root, "job-plan.json")
	writeJobFixture(t, jobPath, e)
	return r, e, jobPath, planPath
}

func writeJobFixture(t *testing.T, path string, e jobEnvelope) {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, string(raw))
}

func TestPrepareJobBindsRawRecipeAndDoesNotExposeCredential(t *testing.T) {
	r, e, jobPath, planPath := jobFixture(t)
	invocation := strings.Repeat("b", 32)
	if err := PrepareJob(jobPath, planPath, invocation); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var plan JobPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.ID != e.Job.ID || plan.Fence != e.Job.Fence || plan.InvocationID != invocation || plan.RecipeSHA256 != e.Job.RecipeSHA256 || plan.Candidate.Status != "unqualified" || plan.Candidate.RecipeSHA256 == e.Job.RecipeSHA256 {
		t.Fatal("preparation lost raw/canonical recipe distinction or job binding")
	}
	if strings.Contains(string(raw), e.Job.Token) || strings.Contains(string(raw), `"token"`) || strings.Contains(string(raw), "training example") {
		t.Fatal("preparation disclosed credentials or training text")
	}
	info, err := os.Stat(planPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("preparation plan is not private", err)
	}
	if entries, err := os.ReadDir(r.OutputDirectory); err != nil || len(entries) != 0 {
		t.Fatal("preparation modified trainer output", err)
	}
	if _, err := os.Lstat(e.TerminalFile); !os.IsNotExist(err) {
		t.Fatal("preparation created terminal evidence", err)
	}
	if err := PrepareJob(jobPath, planPath, invocation); err == nil {
		t.Fatal("overwrote existing job plan")
	}
}

func TestPrepareJobRejectsInvalidLaunchIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*jobEnvelope){
		"version":             func(e *jobEnvelope) { e.Version = 2 },
		"id":                  func(e *jobEnvelope) { e.Job.ID = "bad\nidentity" },
		"token":               func(e *jobEnvelope) { e.Job.Token = "" },
		"fence":               func(e *jobEnvelope) { e.Job.Fence.Epoch = 0 },
		"workload":            func(e *jobEnvelope) { e.Job.Workload = "idle" },
		"revision":            func(e *jobEnvelope) { e.Job.CatalogRevision = "" },
		"status":              func(e *jobEnvelope) { e.Job.Status = "running" },
		"previous invocation": func(e *jobEnvelope) { e.Job.InvocationID = strings.Repeat("a", 32) },
		"terminal":            func(e *jobEnvelope) { e.Job.TerminalOutcome = "completed" },
		"time":                func(e *jobEnvelope) { e.Job.UpdatedAt = time.Time{} },
		"relative recipe":     func(e *jobEnvelope) { e.RecipeFile = "recipe.json" },
		"recipe identity":     func(e *jobEnvelope) { e.Job.RecipeSHA256 = strings.Repeat("c", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			_, e, jobPath, planPath := jobFixture(t)
			mutate(&e)
			writeJobFixture(t, jobPath, e)
			if err := PrepareJob(jobPath, planPath, strings.Repeat("b", 32)); err == nil || (e.Job.Token != "" && strings.Contains(err.Error(), e.Job.Token)) || strings.Contains(err.Error(), jobPath) {
				t.Fatal("accepted invalid job or disclosed private input", err)
			}
			if _, err := os.Lstat(planPath); !os.IsNotExist(err) {
				t.Fatal("rejected job published a plan", err)
			}
		})
	}
}

func TestPrepareJobRequiresExactEnvelopeKeys(t *testing.T) {
	for _, replacement := range []string{`"VERSION":1`, `"version":null`, `"version":1,"version":1`, `"version":1,"unexpected":"secret"`} {
		_, _, jobPath, planPath := jobFixture(t)
		raw, err := os.ReadFile(jobPath)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, jobPath, strings.Replace(string(raw), `"version":1`, replacement, 1))
		if err := PrepareJob(jobPath, planPath, strings.Repeat("b", 32)); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("accepted malformed envelope or disclosed content", err)
		}
	}
}

func TestPrepareJobRejectsInvalidInvocationIdentity(t *testing.T) {
	for _, invocation := range []string{"", strings.Repeat("0", 32), strings.Repeat("A", 32), strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("g", 32)} {
		t.Run(invocation, func(t *testing.T) {
			_, _, jobPath, planPath := jobFixture(t)
			if err := PrepareJob(jobPath, planPath, invocation); err == nil {
				t.Fatal("accepted invalid invocation identity")
			}
			if _, err := os.Lstat(planPath); !os.IsNotExist(err) {
				t.Fatal("invalid invocation published a plan", err)
			}
		})
	}
}

func TestPrepareJobRejectsChangedInputsAndUnsafePaths(t *testing.T) {
	for _, name := range []string{"recipe changed", "dataset changed", "job symlink", "recipe symlink", "parent symlink", "public envelope", "public parent", "writable recipe", "output overlap", "terminal overlap", "invalid invocation"} {
		t.Run(name, func(t *testing.T) {
			r, e, jobPath, planPath := jobFixture(t)
			invocation := strings.Repeat("b", 32)
			switch name {
			case "recipe changed":
				writeFixture(t, e.RecipeFile, "{}")
			case "dataset changed":
				writeFixture(t, r.Train.Path, `{"text":"changed"}`)
			case "job symlink", "recipe symlink":
				path := jobPath
				if name == "recipe symlink" {
					path = e.RecipeFile
				}
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				link := filepath.Join(t.TempDir(), "parent")
				if err := os.Symlink(filepath.Dir(jobPath), link); err != nil {
					t.Fatal(err)
				}
				jobPath = filepath.Join(link, "job.json")
			case "public envelope":
				if err := os.Chmod(jobPath, 0644); err != nil {
					t.Fatal(err)
				}
			case "public parent":
				if err := os.Chmod(filepath.Dir(jobPath), 0755); err != nil {
					t.Fatal(err)
				}
			case "writable recipe":
				if err := os.Chmod(e.RecipeFile, 0620); err != nil {
					t.Fatal(err)
				}
			case "output overlap":
				planPath = filepath.Join(r.OutputDirectory, "plan.json")
			case "terminal overlap":
				planPath = e.TerminalFile
			case "invalid invocation":
				invocation = "not-systemd"
			}
			if err := PrepareJob(jobPath, planPath, invocation); err == nil {
				t.Fatal("accepted unsafe preparation")
			}
		})
	}
}
