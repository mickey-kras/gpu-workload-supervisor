package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/setup"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// fingerprint qualifies an explicit launch file without executing or modifying it.
func (a setupActions) fingerprint(input io.Reader, output io.Writer) error {
	var request struct {
		Binding control.NativeModel `json:"binding"`
	}
	if err := strictjson.DecodeLimited(input, 16384, &request); err != nil {
		return err
	}
	hash, err := a.inspect(request.Binding.LaunchFile, request.Binding)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		SHA256 string `json:"sha256"`
	}{hash})
}

// renderOwned previews the derived owned launch identity and deterministic
// unit rendering for a draft. It writes nothing.
func (a setupActions) renderOwned(home string, input io.Reader, output io.Writer) error {
	var request struct {
		Draft         setup.Draft `json:"draft"`
		ManagerCgroup string      `json:"managerCgroup"`
		SystemctlPath string      `json:"systemctlPath"`
	}
	if err := strictjson.DecodeLimited(input, 16384, &request); err != nil {
		return err
	}
	systemctl := request.SystemctlPath
	if systemctl == "" {
		systemctl = "systemctl"
	}
	actual, err := a.managerCgroup(context.Background(), systemctl)
	if err != nil {
		return err
	}
	if request.ManagerCgroup == "" || request.ManagerCgroup != actual {
		return setup.ErrManagerCgroupMismatch
	}
	profile, raw, err := setup.OwnedProfile(request.Draft, request.ManagerCgroup, home)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		Unit       string                  `json:"unit"`
		Cgroup     string                  `json:"cgroup"`
		LaunchFile string                  `json:"launchFile"`
		SHA256     string                  `json:"sha256"`
		Profile    control.WorkloadProfile `json:"profile"`
	}{profile.Unit, profile.Cgroup, profile.NativeModel.LaunchFile, fmt.Sprintf("%x", sha256.Sum256(raw)), profile})
}
