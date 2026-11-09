package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// The status values of a HostCapStep.
const (
	HostCapNotNeeded = "notNeeded"
	HostCapPresent   = "present"
	HostCapAbsent    = "absent"
	HostCapDifferent = "different"
	HostCapUnknown   = "unknown"
)

// HostCapValues are the slice limits of the host cap drop-in. The memory
// values are MiB, CPUQuotaPercent is percent of one CPU, the bandwidth
// values are bytes per second and the IOPS values are operations per
// second. 0 is unset.
type HostCapValues struct {
	MemoryHighMiB       uint64 `json:"memoryHighMiB"`
	MemoryMaxMiB        uint64 `json:"memoryMaxMiB"`
	TasksMax            uint64 `json:"tasksMax,omitempty"`
	CPUQuotaPercent     uint64 `json:"cpuQuotaPercent,omitempty"`
	IOReadBandwidthMax  uint64 `json:"ioReadBandwidthMax,omitempty"`
	IOWriteBandwidthMax uint64 `json:"ioWriteBandwidthMax,omitempty"`
	IOReadIOPSMax       uint64 `json:"ioReadIOPSMax,omitempty"`
	IOWriteIOPSMax      uint64 `json:"ioWriteIOPSMax,omitempty"`
}

// io gives the I/O caps in the fields of ResourceLimits, so the ioLimits
// table reads them.
func (v HostCapValues) io() control.ResourceLimits {
	return control.ResourceLimits{IOReadBandwidthMax: v.IOReadBandwidthMax, IOWriteBandwidthMax: v.IOWriteBandwidthMax,
		IOReadIOPSMax: v.IOReadIOPSMax, IOWriteIOPSMax: v.IOWriteIOPSMax}
}

// setIO sets the I/O cap of key. An unknown key changes nothing.
func (v *HostCapValues) setIO(key string, value uint64) {
	switch key {
	case "rbps":
		v.IOReadBandwidthMax = value
	case "wbps":
		v.IOWriteBandwidthMax = value
	case "riops":
		v.IOReadIOPSMax = value
	case "wiops":
		v.IOWriteIOPSMax = value
	}
}

// DropIn is one root drop-in of the host cap step. Install tells that the
// root step must write it: the file is missing or differs from Content.
type DropIn struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Install bool   `json:"install,omitempty"`
}

// IOWeightOption gives the I/O weight provider of the device of a unit:
// "bfq", "iocost", "none" when the device supports no I/O weight,
// "invalid" when the device fails the device check, or "unknown" when
// setup cannot check the device.
type IOWeightOption struct {
	Workload control.Workload `json:"workload"`
	Device   string           `json:"device"`
	Provider string           `json:"provider"`
}

// HostCapStep is the host cap step of setup: the status of the host cap,
// the exact root drop-ins and commands, the problems that the runtime
// checks found and the I/O weight option of each device. Setup never runs
// the commands.
type HostCapStep struct {
	Status   string           `json:"status"`
	UID      int              `json:"uid,omitempty"`
	Values   *HostCapValues   `json:"values,omitempty"`
	DropIns  []DropIn         `json:"dropIns,omitempty"`
	Commands []string         `json:"commands,omitempty"`
	Problems []string         `json:"problems,omitempty"`
	IOWeight []IOWeightOption `json:"ioWeight,omitempty"`
	Reserve  LimitReserve     `json:"reserve"`
}

// CheckLimitHost runs the setup checks of a unit with limits: the capped
// controls, the controllers, the host cap, the reserve and the I/O device.
// These are the launch checks of the runtime without the binding and the
// loaded properties, because setup runs them before it writes the unit.
// A unit without limits passes.
func (m *SystemdManager) CheckLimitHost(ctx context.Context, workload control.Workload) error {
	p, limited, err := m.limitedProfile(workload)
	if err != nil || !limited {
		return err
	}
	_, _, err = m.checkLimitHost(ctx, p.NativeModel.Owned.Limits)
	return err
}

// HostCapStep reports the host cap step for the catalog of the runtime.
// want are the slice values that setup requests; nil gives the status and
// the drop-in paths only. An error means that want cannot hold the units
// with their reserve.
func (m *SystemdManager) HostCapStep(ctx context.Context, want *HostCapValues) (HostCapStep, error) {
	step := HostCapStep{Status: HostCapNotNeeded, Reserve: m.reserve}
	limited := m.config.Catalog.LimitedProfiles()
	if len(limited) == 0 {
		return step, nil
	}
	step.UID = m.host.uid
	var limits []control.ResourceLimits
	for _, p := range limited {
		limits = append(limits, p.NativeModel.Owned.Limits)
	}
	content := map[string]string{}
	if want != nil {
		values, device, err := m.reserve.HostCapFor(*want, limits)
		if err != nil {
			return step, err
		}
		step.Values = &values
		content[m.host.capDropIn()] = renderSliceCap(values, device)
	}
	paths := []string{m.host.capDropIn()}
	if slicesUseIO(limits) {
		paths = append(paths, m.host.delegateDropIn())
		if want != nil {
			content[m.host.delegateDropIn()] = "[Service]\nDelegate=io\n"
		}
	}
	files := m.compareDropIns(&step, paths, content)
	checks := m.checkLimitedHosts(ctx, &step, limited)
	if want != nil && files == stepOK && checks != stepOK {
		// The drop-ins match, but systemd can still load the old ones.
		step.Problems = append(step.Problems, "the drop-ins match the requested values; after a new install, run `sudo systemctl daemon-reload`")
	}
	step.Status = hostCapStatus(files, checks)
	step.Commands = hostCapCommands(step.DropIns, m.host.delegateDropIn(), m.host.managerUnit())
	return step, nil
}

func slicesUseIO(limits []control.ResourceLimits) bool {
	for _, l := range limits {
		if l.UsesIO() {
			return true
		}
	}
	return false
}

// The results of compareDropIns and checkLimitedHosts, from good to bad.
const (
	stepOK = iota
	stepUnknown
	stepDifferent
	stepAbsent
)

// compareDropIns adds each drop-in to step and compares its installed file
// with content, when content names it. The delegation drop-in matches when
// it delegates io, whatever its text, so it asks for no needless restart
// of the user manager. Without content, a drop-in to install gets a hint.
func (m *SystemdManager) compareDropIns(step *HostCapStep, paths []string, content map[string]string) int {
	result := stepOK
	hint := false
	for _, name := range paths {
		dropIn := DropIn{Path: name, Content: content[name]}
		data, err := os.ReadFile(m.host.path(name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			step.Problems = append(step.Problems, name+" is missing")
			result, dropIn.Install = max(result, stepAbsent), true
		case err != nil:
			step.Problems = append(step.Problems, fmt.Sprintf("%s cannot be read: %v", name, err))
			result = max(result, stepUnknown)
		case dropIn.Content != "" && !m.dropInMatches(name, string(data), dropIn.Content):
			step.Problems = append(step.Problems, name+" differs from the requested values")
			result, dropIn.Install = max(result, stepDifferent), true
		}
		hint = hint || (dropIn.Install && dropIn.Content == "")
		step.DropIns = append(step.DropIns, dropIn)
	}
	if hint {
		step.Problems = append(step.Problems, "set hostCap.memoryHighMiB and hostCap.memoryMaxMiB in the setup request to get the root commands")
	}
	return result
}

// dropInMatches compares an installed drop-in with its content. The
// delegation drop-in matches when its [Service] section delegates io.
func (m *SystemdManager) dropInMatches(name, installed, content string) bool {
	if name == m.host.delegateDropIn() {
		return delegatesIO(installed)
	}
	return installed == content
}

// delegatesIO reports a [Service] section whose Delegate= settings
// delegate io, parsed as systemd parses them. A line splits at its first
// "=", and both sides are trimmed. Comments and blank lines do not count.
// A boolean true delegates every controller and a boolean false clears
// the delegation. An empty value clears the list. Any other value is a
// controller list, which adds to the earlier lines.
func delegatesIO(text string) bool {
	section := ""
	delegated := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "["):
			section = line
		case section == "[Service]":
			key, value, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(key) == "Delegate" {
				delegated = delegateValue(strings.TrimSpace(value), delegated)
			}
		}
	}
	return delegated
}

// delegateValue applies one Delegate= value to the earlier result.
func delegateValue(value string, delegated bool) bool {
	switch strings.ToLower(value) {
	case "1", "yes", "y", "true", "t", "on":
		return true
	case "0", "no", "n", "false", "f", "off", "":
		return false
	}
	return delegated || slices.Contains(strings.Fields(value), "io")
}

// checkLimitedHosts runs the setup checks of each unit with limits and adds
// the I/O weight option of each device.
func (m *SystemdManager) checkLimitedHosts(ctx context.Context, step *HostCapStep, limited []control.WorkloadProfile) int {
	result := stepOK
	for _, p := range limited {
		l := p.NativeModel.Owned.Limits
		if _, _, err := m.checkLimitHost(ctx, l); err != nil {
			step.Problems = append(step.Problems, string(p.ID)+": "+err.Error())
			severity := stepUnknown
			if DefiniteLimitFailure(err) {
				severity = stepDifferent
			}
			result = max(result, severity)
		}
		if l.UsesIO() {
			step.IOWeight = append(step.IOWeight, IOWeightOption{Workload: p.ID, Device: l.IODevice, Provider: m.ioWeightOption(l)})
		}
	}
	return result
}

// ioWeightOption gives the I/O weight provider of the device of l, "none"
// when the device supports no I/O weight, "invalid" when the device fails
// the device check, or "unknown" when the device cannot be checked.
func (m *SystemdManager) ioWeightOption(l control.ResourceLimits) string {
	l.IOWeight = 1
	provider, err := m.verifyLimitDevice(l)
	var none *noIOWeightError
	switch {
	case err == nil:
		return provider
	case errors.As(err, &none):
		return "none"
	case DefiniteLimitFailure(err):
		return "invalid"
	default:
		return "unknown"
	}
}

// hostCapStatus gives the status from the drop-in files and the checks. A
// missing drop-in makes the cap absent, whatever the checks found.
func hostCapStatus(files, checks int) string {
	switch max(files, checks) {
	case stepAbsent:
		return HostCapAbsent
	case stepDifferent:
		return HostCapDifferent
	case stepUnknown:
		return HostCapUnknown
	default:
		return HostCapPresent
	}
}

// renderSliceCap gives the content of the slice drop-in.
func renderSliceCap(v HostCapValues, device string) string {
	lines := []string{"[Slice]",
		"MemoryHigh=" + strconv.FormatUint(v.MemoryHighMiB, 10) + "M",
		"MemoryMax=" + strconv.FormatUint(v.MemoryMaxMiB, 10) + "M",
		"MemorySwapMax=0",
		"TasksMax=" + strconv.FormatUint(v.TasksMax, 10),
		"CPUQuota=" + strconv.FormatUint(v.CPUQuotaPercent, 10) + "%"}
	for _, limit := range ioLimits {
		if value := limit.value(v.io()); value != 0 {
			lines = append(lines, limit.property+"="+device+" "+strconv.FormatUint(value, 10))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// hostCapCommands gives the root commands that install the drop-ins that
// need it and have content, with explicit modes. Setup shows them and never
// runs them. The delegation drop-in at delegatePath takes effect only after
// a restart of the user manager, which stops the supervisor and its units.
func hostCapCommands(dropIns []DropIn, delegatePath, manager string) []string {
	var commands []string
	delegate := false
	for _, dropIn := range dropIns {
		if dropIn.Content == "" || !dropIn.Install {
			continue
		}
		commands = append(commands, "sudo install -d -m 0755 "+path.Dir(dropIn.Path),
			"sudo install -m 0644 /dev/stdin "+dropIn.Path+" <<'EOF'\n"+dropIn.Content+"EOF")
		delegate = delegate || dropIn.Path == delegatePath
	}
	if len(commands) == 0 {
		return nil
	}
	commands = append(commands, "sudo systemctl daemon-reload")
	if delegate {
		commands = append(commands, "sudo systemctl restart "+manager)
	}
	return commands
}
