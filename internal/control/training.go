package control

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// TrainerBinding describes a trusted local finite-job adapter. The adapter owns
// datasets, training configuration, evaluation and export, inside one unit.
type TrainerBinding struct {
	Version          int    `json:"version"`
	GPUUUID          string `json:"gpuUUID"`
	LaunchFile       string `json:"launchFile"`
	LaunchSHA256     string `json:"launchSHA256"`
	Executable       string `json:"executable"`
	ExecutableSHA256 string `json:"executableSHA256"`
	RecipeFile       string `json:"recipeFile"`
	RecipeSHA256     string `json:"recipeSHA256"`
	JobFile          string `json:"jobFile"`
	TerminalFile     string `json:"terminalFile"`
}

func (t TrainerBinding) Validate() error {
	if t.Version != 1 || !ValidGPUUUID(t.GPUUUID) {
		return errors.New("unsupported trainer binding or invalid GPU UUID")
	}
	paths := []string{t.LaunchFile, t.Executable, t.RecipeFile, t.JobFile, t.TerminalFile}
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "/" || strings.IndexFunc(p, unicode.IsControl) >= 0 || !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, " \t\r\n\\$%`\"';|&<>") || seen[p] {
			return errors.New("trainer requires distinct clean literal absolute paths")
		}
		seen[p] = true
	}
	for _, digest := range []string{t.LaunchSHA256, t.ExecutableSHA256, t.RecipeSHA256} {
		raw, err := hex.DecodeString(digest)
		if err != nil || len(raw) != 32 {
			return errors.New("trainer requires SHA256 evidence")
		}
	}
	return nil
}

type TrainingJob struct {
	TerminalOutcome string    `json:"terminalOutcome,omitempty"`
	ID              string    `json:"id"`
	Token           string    `json:"token"`
	Workload        Workload  `json:"workload"`
	Fence           Fence     `json:"fence"`
	CatalogRevision string    `json:"catalogRevision"`
	RecipeSHA256    string    `json:"recipeSHA256"`
	InvocationID    string    `json:"invocationId"`
	Status          string    `json:"status"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type TrainingTerminal struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	Token        string `json:"token"`
	Fence        Fence  `json:"fence"`
	RecipeSHA256 string `json:"recipeSHA256"`
	InvocationID string `json:"invocationId"`
	Outcome      string `json:"outcome"`
}

func TrainingTerminalOutcome(outcome string) bool {
	return outcome == "completed" || outcome == "failed" || outcome == "cancelled" || outcome == "interrupted"
}

func (e TrainingTerminal) Matches(j TrainingJob) bool {
	return e.Version == 1 && e.ID == j.ID && e.Token == j.Token && e.Fence == j.Fence && e.RecipeSHA256 == j.RecipeSHA256 && e.InvocationID == j.InvocationID && j.InvocationID != "" && TrainingTerminalOutcome(e.Outcome)
}

func trainerOutputCollision(a, b WorkloadProfile) bool {
	if a.Trainer == nil {
		return false
	}
	for _, output := range []string{a.Trainer.JobFile, a.Trainer.TerminalFile} {
		for _, source := range boundProfileFiles(b) {
			if output == source {
				return true
			}
		}
	}
	return false
}

func boundProfileFiles(p WorkloadProfile) []string {
	var paths []string
	var drops []LaunchSource
	if t := p.Trainer; t != nil {
		paths = append(paths, t.JobFile, t.TerminalFile, t.RecipeFile, t.LaunchFile, t.Executable)
	}
	if n := p.NativeModel; n != nil {
		paths = append(paths, n.LaunchFile, n.Model)
		drops = n.DropIns
		if n.Owned != nil {
			paths = append(paths, n.Owned.Executable, n.Owned.ModelPath)
		}
	}
	if b := p.LaunchBinding; b != nil {
		paths = append(paths, b.LaunchFile)
		drops = b.DropIns
	}
	for _, d := range drops {
		paths = append(paths, d.Path)
	}
	return paths
}

func (p WorkloadProfile) validateTrainerProfile() error {
	if p.NativeModel != nil || p.LaunchBinding != nil || p.Adapter != "systemd" || p.HealthURL != "" || p.ReleaseURL != "" || p.BootPolicy != "stop-to-idle" || p.RequiredMiB == 0 || p.SystemdSlice != "" || p.SystemdVersion != 0 {
		return errors.New("invalid finite trainer profile")
	}
	if err := p.Trainer.Validate(); err != nil {
		return err
	}
	if err := p.validateNaming(); err != nil {
		return err
	}
	return p.validatePlacement()
}
