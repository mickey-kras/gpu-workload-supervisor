package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

// TrainerUnit is intentionally a separate, exact launch grammar. Adapter v1
// reads the lifecycle envelope, owns all GPU phases and writes terminal evidence.
func TrainerUnit(t control.TrainerBinding) string {
	return fmt.Sprintf("[Unit]\nDescription=Supervised finite training\n[Service]\nType=exec\nRestart=no\nKillMode=control-group\nRemainAfterExit=yes\nEnvironment=CUDA_VISIBLE_DEVICES=%s\nExecStart=%s --job-file %s\n", t.GPUUUID, t.Executable, t.JobFile)
}

func (m *SystemdManager) trainerProperties(ctx context.Context, p control.WorkloadProfile) (map[string]string, error) {
	out, err := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "show", "--property=LoadState,ActiveState,SubState,ControlGroup,InvocationID,Result,ExecMainCode,ExecMainStatus,FragmentPath,DropInPaths,NeedDaemonReload,ExecStart,ExecStartPre,ExecStartPost,ExecStop,ExecStopPost,Environment,Type,Restart,KillMode,RemainAfterExit", "--", p.Unit)
	if err != nil {
		return nil, SafeError("trainer observation failed", err)
	}
	return ParseUnitProperties(out)
}

func verifyTrainerDigest(path, digest string) error {
	raw, err := readLaunchSource(path)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != digest {
		return ErrLaunchChanged
	}
	return nil
}

func verifyTrainerExecutable(t control.TrainerBinding) error {
	resolved, err := validateExecutable(t.Executable)
	if err != nil {
		return err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, 256*1024*1024+1))
	if err != nil {
		return err
	}
	if n > 256*1024*1024 || fmt.Sprintf("%x", h.Sum(nil)) != t.ExecutableSHA256 {
		return ErrLaunchChanged
	}
	return nil
}

func (m *SystemdManager) verifyTrainerBinding(ctx context.Context, p control.WorkloadProfile) error {
	t := p.Trainer
	if t == nil {
		return ErrLaunchUnsupported
	}
	if err := qualifyTrainingOutputs(*t); err != nil {
		return err
	}
	if err := verifyTrainerExecutable(*t); err != nil {
		return err
	}
	if err := verifyTrainerDigest(t.RecipeFile, t.RecipeSHA256); err != nil {
		return err
	}
	raw, err := readLaunchSource(t.LaunchFile)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, []byte(TrainerUnit(*t))) || fmt.Sprintf("%x", sha256.Sum256(raw)) != t.LaunchSHA256 {
		return ErrLaunchChanged
	}
	v, err := m.trainerProperties(ctx, p)
	if err != nil {
		return err
	}
	for key, want := range map[string]string{
		"LoadState":        "loaded",
		"NeedDaemonReload": "no",
		"Type":             "exec",
		"Restart":          "no",
		"KillMode":         "control-group",
		"RemainAfterExit":  "yes",
		"ExecStartPre":     "",
		"ExecStartPost":    "",
		"ExecStop":         "",
		"ExecStopPost":     "",
	} {
		if v[key] != want {
			return ErrLaunchChanged
		}
	}
	if err := CheckNativeBindingSources(v, p.Unit, t.LaunchFile, nil); err != nil {
		return err
	}
	if err := CheckLoadedLaunchCommand(v["ExecStart"], t.Executable+" --job-file "+t.JobFile); err != nil {
		return err
	}
	if err := CheckLoadedEnvironment(v["Environment"], map[string]string{"CUDA_VISIBLE_DEVICES": t.GPUUUID}); err != nil {
		return err
	}
	return m.verifyProfileGPU(ctx, p)
}

func qualifyTrainingOutputs(t control.TrainerBinding) error {
	for _, path := range []string{t.JobFile, t.TerminalFile} {
		if err := qualifyEnvironmentPath(filepath.Dir(path), false, false); err != nil {
			return err
		}
		if err := unix.Faccessat(unix.AT_FDCWD, filepath.Dir(path), unix.W_OK|unix.X_OK, unix.AT_EACCESS); err != nil {
			return ErrLaunchUnsupported
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := qualifyEnvironmentPath(path, true, false); err != nil {
			return err
		}
	}
	return nil
}

func (m *SystemdManager) StartTraining(ctx context.Context, p control.WorkloadProfile, job control.TrainingJob) (string, error) {
	if err := m.verifyTrainerBinding(ctx, p); err != nil {
		return "", err
	}
	if err := m.ReleasedFor(ctx, control.WorkloadIdle); err != nil {
		return "", err
	}
	if err := m.capacity(ctx, p.ID); err != nil {
		return "", err
	}
	prior, err := m.trainerProperties(ctx, p)
	if err != nil {
		return "", err
	}
	if prior["ActiveState"] != "inactive" || prior["SubState"] != "dead" {
		return "", errors.New("trainer is not stopped")
	}
	t := p.Trainer
	// Recheck after the handoff: destinations may have changed since preflight.
	if err := qualifyTrainingOutputs(*t); err != nil {
		return "", err
	}
	envelope := struct {
		Version      int                 `json:"version"`
		Job          control.TrainingJob `json:"job"`
		RecipeFile   string              `json:"recipeFile"`
		TerminalFile string              `json:"terminalFile"`
	}{1, job, t.RecipeFile, t.TerminalFile}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	if err := writeTrainingEnvelope(t.JobFile, raw); err != nil {
		return "", err
	}
	// The committed launch intent is single-use. Never retry this command after
	// uncertainty: explicit recovery stops and verifies before another job.
	_, startErr := m.runner.Run(ctx, m.config.SystemctlPath, "--user", "--job-mode=fail", "start", "--", p.Unit)
	v, err := m.trainerProperties(ctx, p)
	if err != nil {
		return "", errors.Join(startErr, err)
	}
	id := v["InvocationID"]
	if !validInvocationID(id) || id == prior["InvocationID"] || v["ControlGroup"] != p.Cgroup {
		return "", errors.Join(startErr, errors.New("trainer invocation is uncertain; explicit recovery required"))
	}
	return id, startErr
}

func (m *SystemdManager) TrainingTerminal(ctx context.Context, p control.WorkloadProfile, job control.TrainingJob) (control.TrainingTerminal, error) {
	var evidence control.TrainingTerminal
	v, err := m.trainerProperties(ctx, p)
	if err != nil {
		return evidence, err
	}
	if job.InvocationID == "" || v["InvocationID"] != job.InvocationID {
		return evidence, ErrLaunchChanged
	}
	if v["ActiveState"] != "failed" && !(v["ActiveState"] == "active" && v["SubState"] == "exited") {
		return evidence, errors.New("trainer has not exited")
	}
	// A fenced systemd failure is terminal evidence, but still needs recursive
	// cgroup and GPU release verification before admission may be completed.
	if v["ActiveState"] == "failed" && (v["Result"] == "oom-kill" || v["Result"] == "exit-code" || v["Result"] == "signal" || v["Result"] == "core-dump") {
		return control.TrainingTerminal{Version: 1, ID: job.ID, Token: job.Token, Fence: job.Fence, RecipeSHA256: job.RecipeSHA256, InvocationID: job.InvocationID, Outcome: "failed"}, nil
	}
	raw, err := readLaunchSource(p.Trainer.TerminalFile)
	if err != nil {
		return evidence, err
	}
	// Allow the full request ID contract, including up to six JSON bytes per
	// ID byte, while retaining a bounded strict terminal receipt.
	if err := strictjson.DecodeLimited(bytes.NewReader(raw), 64<<10, &evidence); err != nil {
		return evidence, SafeError("invalid trainer terminal evidence", err)
	}
	if !evidence.Matches(job) {
		return evidence, errors.New("stale or invalid trainer terminal evidence")
	}

	// Exit zero supports a completed acknowledgement but never substitutes for it.
	if evidence.Outcome == "completed" && (v["Result"] != "success" || v["ExecMainCode"] != "1" || v["ExecMainStatus"] != "0") {
		return evidence, errors.New("trainer completion conflicts with process result")
	}
	return evidence, nil
}

func (m *SystemdManager) ReleaseTraining(ctx context.Context, p control.WorkloadProfile) error {
	if err := m.releasedUnit(ctx, p.Unit, p.Cgroup, false); err != nil {
		return err
	}
	if err := m.verifyProfileGPU(ctx, p); err != nil {
		return err
	}
	out, err := m.runner.Run(ctx, m.config.NvidiaSMIPath, "--query-compute-apps=pid", "--format=csv,noheader,nounits", "-i", p.Trainer.GPUUUID)
	if err != nil {
		return SafeError("trainer GPU release observation failed", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return errors.New("GPU compute processes remain after trainer exit")
	}
	return m.capacity(ctx, p.ID)
}

// The parent is qualified before this atomic replacement; the envelope contains
// lifecycle identity only and is never used as a script or shell fragment.
func writeTrainingEnvelope(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".training-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
