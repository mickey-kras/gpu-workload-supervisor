package runtime

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

// systemd starts a unit even when it cannot apply a limit, and it reports the
// configured values while the cgroup holds others. A unit with limits starts
// only after the escape route, binding, controller, host cap, reserve,
// device, loaded property and memory checks pass, and it stays healthy only
// while its cgroup holds the exact values. Each error class names what
// failed.
var (
	ErrLimitController = errors.New("a required cgroup controller is not enabled for this account")
	ErrLimitInterface  = errors.New("the system cannot apply a resource limit")
	ErrHostCap         = errors.New("the host protection of this account is missing or different")
	// ErrLimitReserve covers a limit above its slice cap minus the reserve
	// and a required limit that is unset: both let the unit use the room
	// of the supervisor. It also covers a slice io.max cap on a device
	// other than the unit ioDevice, which the unit cannot cap.
	ErrLimitReserve    = errors.New("a resource limit is unset or leaves too little room for the supervisor")
	ErrLimitMemory     = errors.New("slice memory does not fit this limit")
	ErrLimitDevice     = errors.New("the I/O device of the resource limits is not usable")
	ErrLimitProperties = errors.New("the loaded unit limits do not match the catalog")
	ErrLimitDrift      = errors.New("a resource limit changed after start")
	ErrLimitReadback   = errors.New("resource limit readback failed")
	// ErrLimitCheckFailed marks a limit check of a started unit that proves
	// no limit: a mismatch, or a failed read of a limit file of a running
	// unit. The supervisor treats it as definite: a failed transition
	// stops the unit (through its rollback from a supervisor-owned source,
	// or with its own stop from a user-owned source), reconciliation does
	// not keep it, and a status check latches the error state.
	ErrLimitCheckFailed = errors.New("a resource limit check of a started unit failed")
	// ErrLimitUnchecked marks a runtime that cannot run the limit checks.
	ErrLimitUnchecked = errors.New("the runtime cannot check resource limits")
	// ErrEscapeRoute marks an open escape route: a program of the service
	// account can run outside the host cap. The text names each route. A
	// check that cannot run is ErrLimitReadback instead.
	ErrEscapeRoute = errors.New("programs of this account can run outside host protection")
)

// errBindingCommand marks a binding check whose command failed: the
// systemctl call of the launch binding or the nvidia-smi query of the GPU
// identity. It proves no change.
var errBindingCommand = errors.New("a binding check command failed")

// errBindingRead marks a binding check whose file read failed with an I/O
// error other than ENOENT, ELOOP or ENOTDIR: the launch file or a drop-in,
// the model status, a path of the trust walk, the chat template or the API
// key file. It proves no change.
var errBindingRead = errors.New("a binding check read failed")

// bindingReadFailure reports an I/O error that proves nothing about the
// file. As for the host cap files, a missing chat template or API key file,
// a refused symlink and a file where a directory must be are definite. A
// missing model keeps ErrModelUnavailable, which is not a limit refusal.
func bindingReadFailure(err error) bool {
	var errno unix.Errno
	return errors.As(err, &errno) && !definiteFileErrno(err)
}

// definiteFileErrno reports the file errors that prove a mismatch, for the
// binding files and the host files alike: a missing file, a refused symlink
// and a file where a directory must be.
func definiteFileErrno(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}

// markBindingRead returns failure, marked with errBindingRead when cause is
// such a read failure or already has the mark. The text stays the same.
func markBindingRead(failure, cause error) error {
	if errors.Is(cause, errBindingRead) || bindingReadFailure(cause) {
		return SafeError(failure.Error(), failure, errBindingRead)
	}
	return failure
}

// DefiniteBindingFailure reports a launch binding that is proven changed or
// unsupported. A failed command, read or context proves nothing. A launch
// error that groups several problems is definite when one of them is: a
// read failure next to a proven mismatch does not hide the mismatch.
func DefiniteBindingFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var group *launchDiagnosticError
	if errors.As(err, &group) {
		return slices.ContainsFunc(group.items, DefiniteBindingFailure)
	}
	if errors.Is(err, errBindingCommand) || errors.Is(err, errBindingRead) {
		return false
	}
	return errors.Is(err, ErrLaunchChanged) || errors.Is(err, ErrLaunchUnsupported)
}

// DefiniteLimitFailure reports a check that found a definite mismatch.
// Command, read and context failures are not definite: a later probe can
// pass, so callers retry them or fail closed without a stop.
func DefiniteLimitFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A readback failure of a running unit is tagged ErrLimitCheckFailed:
	// no limit is proven. An untagged readback failure is not definite.
	if errors.Is(err, ErrLimitCheckFailed) {
		return true
	}
	if errors.Is(err, ErrLimitReadback) {
		return false
	}
	for _, definite := range []error{ErrEscapeRoute, ErrHostCap, ErrLimitController, ErrLimitInterface, ErrLimitReserve, ErrLimitDevice, ErrLimitProperties, ErrLimitDrift, ErrLimitUnchecked} {
		if errors.Is(err, definite) {
			return true
		}
	}
	return false
}

// tagStarted marks a definite failure of a started unit with
// ErrLimitCheckFailed and returns other failures as they are.
func tagStarted(err error) error {
	if DefiniteLimitFailure(err) {
		return checkFailed(err)
	}
	return err
}

// checkFailed marks err with ErrLimitCheckFailed. Callers use it directly
// only for a failure that is definite for a started unit but not by its
// own class: a changed binding or a failed limit file read.
func checkFailed(err error) error {
	return fmt.Errorf("%w: %w", ErrLimitCheckFailed, err)
}

// StartedCause returns the cause of an error that checkFailed tagged with
// ErrLimitCheckFailed as its outermost wrap, and true. Other errors come back
// as they are, with false. It works on the error values, not on their text.
func StartedCause(err error) (error, bool) {
	tagged, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err, false
	}
	causes := tagged.Unwrap()
	if len(causes) != 2 || causes[0] != ErrLimitCheckFailed {
		return err, false
	}
	return causes[1], true
}

const mebibyte = 1 << 20

// launchChecks runs the escape route checks, the binding check and the host
// protection checks of a unit with limits, in that order. Start and
// VerifyLaunchLimits both run it, so the two lists stay the same. It does
// not change the unit, so a failure leaves the host as it was. The
// returned cap holds the slice ceilings for the memory check of Start.
func (m *SystemdManager) launchChecks(ctx context.Context, p control.WorkloadProfile) (hostCap, error) {
	if err := m.escapeRoutes(ctx, p); err != nil {
		return hostCap{}, err
	}
	if err := m.verifyNativeBinding(ctx, p); err != nil {
		return hostCap{}, err
	}
	limitCap, _, err := m.checkHostProtection(ctx, p)
	return limitCap, err
}

// limitedProfile returns the profile of a unit with limits. ok is false for
// a unit without limits. A workload that is not in the runtime catalog
// gives ErrLimitUnchecked: the supervisor asks only after its own catalog
// shows limits, so the check fails closed.
func (m *SystemdManager) limitedProfile(workload control.Workload) (p control.WorkloadProfile, ok bool, err error) {
	p, found := m.config.Catalog.Profile(workload)
	if !found {
		return p, false, fmt.Errorf("%w: %s is not in the runtime catalog", ErrLimitUnchecked, workload)
	}
	return p, p.HasLimits(), nil
}

// VerifyLaunchLimits runs the launch checks of a unit with limits, the
// escape route checks first. The supervisor calls it before it unloads the
// current workload, so a failure
// refuses the switch with no change. It leaves out the slice memory check:
// the current workload still holds its memory there. Start runs the same
// checks and also the memory check.
func (m *SystemdManager) VerifyLaunchLimits(ctx context.Context, workload control.Workload) error {
	p, limited, err := m.limitedProfile(workload)
	if err != nil || !limited {
		return err
	}
	_, err = m.launchChecks(ctx, p)
	return err
}

// VerifyLimitProtection runs the same checks as a health check of a running
// unit with limits: the escape route checks, the binding, the host protection
// checks and the cgroup readback. The supervisor calls it before it
// retains a running unit. It leaves out the slice memory check, because the running unit
// already holds its memory in the slice. It changes nothing.
func (m *SystemdManager) VerifyLimitProtection(ctx context.Context, workload control.Workload) error {
	p, limited, err := m.limitedProfile(workload)
	if err != nil || !limited {
		return err
	}
	return m.checkStartedLimitedUnit(ctx, p)
}

// checkStartedUnit checks the binding of a started unit, and for a unit
// with limits also its limits. A limit error names the workload and unit.
func (m *SystemdManager) checkStartedUnit(ctx context.Context, p control.WorkloadProfile) error {
	if !p.HasLimits() {
		return m.verifyNativeBinding(ctx, p)
	}
	if err := m.checkStartedLimitedUnit(ctx, p); err != nil {
		return UnitError(p, err)
	}
	return nil
}

// checkStartedLimitedUnit checks the binding and the limits of a started
// unit with limits. A changed binding, such as a set-property drop-in, is a
// definite limit failure.
func (m *SystemdManager) checkStartedLimitedUnit(ctx context.Context, p control.WorkloadProfile) error {
	// The escape route checks run first, so an open route always gives the
	// definite refusal, whatever the binding probe returns.
	if err := m.escapeRoutes(ctx, p); err != nil {
		return tagStarted(err)
	}
	if err := m.verifyNativeBinding(ctx, p); err != nil {
		if DefiniteBindingFailure(err) {
			return checkFailed(err)
		}
		return err
	}
	return m.verifyStartedLimits(ctx, p)
}

// checkHostProtection checks the controllers, the host cap, the reserve,
// the I/O device and the loaded properties. Its callers run the escape
// route checks first. It returns the slice ceilings and the I/O weight
// provider of the device for the readback: "bfq", "iocost", or empty when
// IOWeight is not set.
func (m *SystemdManager) checkHostProtection(ctx context.Context, p control.WorkloadProfile) (limitCap hostCap, provider string, err error) {
	l := p.NativeModel.Owned.Limits
	if err := requireCappedControls(l); err != nil {
		return hostCap{}, "", err
	}
	managerRoot, err := m.managerCgroup(ctx)
	if err != nil {
		return hostCap{}, "", err
	}
	if err := m.requireControllers(managerRoot, l, m.managerControllerRemedy); err != nil {
		return hostCap{}, "", err
	}
	if err := m.requireManagerFiles(managerRoot, l); err != nil {
		return hostCap{}, "", err
	}
	if limitCap, err = m.verifyHostCap(ctx, managerRoot, l); err != nil {
		return hostCap{}, "", err
	}
	if err := checkReserve(l, limitCap); err != nil {
		return hostCap{}, "", err
	}
	if provider, err = m.verifyLimitDevice(l); err != nil {
		return hostCap{}, "", err
	}
	return limitCap, provider, m.verifyLoadedLimits(ctx, p.Unit, l)
}

// requireManagerFiles checks the interface files at the user manager root
// that the readback of the unit needs. Each host protection check runs it.
// The readback reads memory.swap.max whenever the memory controller is in
// use, and MemoryMax renders MemorySwapMax. Only iocost gives the io.weight
// file, and the readback reads it also when BFQ applies the weight.
func (m *SystemdManager) requireManagerFiles(root string, l control.ResourceLimits) error {
	var files []limitFile
	if l.UsesMemory() {
		files = append(files, limitFile{name: "memory.swap.max", feature: "swap accounting"})
	}
	if l.IOWeight != 0 {
		files = append(files, limitFile{name: "io.weight", feature: "iocost"})
	}
	for _, file := range files {
		_, err := m.readLimitFile(root, file)
		if errors.Is(err, errControlFileAbsent) {
			return missingControlFile(root, file)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// limitPropertyNames are the loaded properties of a unit with limits, in the
// order of the systemctl show request. expectedLimitProperties gives them.
var limitPropertyNames = func() []string {
	var names []string
	for _, property := range expectedLimitProperties(control.ResourceLimits{}) {
		names = append(names, property.name)
	}
	return names
}()

// limitProperty is one loaded property and its expected lines.
type limitProperty struct {
	name string
	want []string
}

// formatMiB renders a memory limit in bytes, or unset when it is 0.
func formatMiB(mib uint64, unset string) string {
	if mib == 0 {
		return unset
	}
	return strconv.FormatUint(mib*mebibyte, 10)
}

// formatSwap renders the swap limit in bytes, or unset without MemoryMax.
func formatSwap(l control.ResourceLimits, unset string) string {
	if v, ok := l.SwapLimitMiB(); ok {
		return strconv.FormatUint(v*mebibyte, 10)
	}
	return unset
}

// expectedLimitProperties gives each loaded property as systemctl show
// prints it, in the order of the show request. A device limit list has one
// entry per device. An unset memory or CPU quota renders as the systemd
// default. TasksMax has no such form, because its default comes from the
// DefaultTasksMax of the manager: requireCappedControls refuses an unset
// tasksMax before any check reads these values.
func expectedLimitProperties(l control.ResourceLimits) []limitProperty {
	weight := func(v uint64) []string {
		if v == 0 {
			return []string{"[not set]"}
		}
		return []string{strconv.FormatUint(v, 10)}
	}
	device := func(v uint64) []string {
		if v == 0 {
			return nil
		}
		return []string{l.IODevice + " " + strconv.FormatUint(v, 10)}
	}
	quota := []string{"infinity"}
	if l.CPUQuotaPercent != 0 {
		quota = []string{formatTimespan(l.CPUQuotaPercent * 10000)}
	}
	return []limitProperty{
		{"OOMPolicy", []string{"stop"}}, {"OOMScoreAdjust", []string{"500"}},
		{"MemoryHigh", []string{formatMiB(l.MemoryHighMiB, "infinity")}}, {"MemoryMax", []string{formatMiB(l.MemoryMaxMiB, "infinity")}},
		{"MemorySwapMax", []string{formatSwap(l, "infinity")}}, {"TasksMax", []string{strconv.FormatUint(l.TasksMax, 10)}},
		{"CPUWeight", weight(l.CPUWeight)}, {"CPUQuotaPerSecUSec", quota}, {"IOWeight", weight(l.IOWeight)},
		{"IOReadBandwidthMax", device(l.IOReadBandwidthMax)}, {"IOWriteBandwidthMax", device(l.IOWriteBandwidthMax)},
		{"IOReadIOPSMax", device(l.IOReadIOPSMax)}, {"IOWriteIOPSMax", device(l.IOWriteIOPSMax)},
	}
}

// verifyLoadedLimits compares the loaded unit properties with the catalog.
// verifyNativeBinding separately requires no drop-ins and NeedDaemonReload=no.
func (m *SystemdManager) verifyLoadedLimits(ctx context.Context, unit string, l control.ResourceLimits) error {
	values, err := m.showProperties(ctx, true, unit, limitPropertyNames)
	if err != nil {
		return err
	}
	for _, property := range expectedLimitProperties(l) {
		if got := values[property.name]; !slices.Equal(got, property.want) {
			return fmt.Errorf("%w: %s is %q, not %q", ErrLimitProperties, property.name, strings.Join(got, ", "), strings.Join(property.want, ", "))
		}
	}
	return nil
}

// showProperties reads properties of one unit. A property can have several
// lines, such as one per device; an empty value counts as no line.
func (m *SystemdManager) showProperties(ctx context.Context, user bool, unit string, names []string) (map[string][]string, error) {
	var args []string
	if user {
		args = append(args, "--user")
	}
	args = append(args, "show")
	for _, name := range names {
		args = append(args, "--property="+name)
	}
	output, err := m.runner.Run(ctx, m.config.SystemctlPath, append(args, "--", unit)...)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", unit, SafeError("command failed", err))
	}
	values := map[string][]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && value != "" {
			values[key] = append(values[key], value)
		}
	}
	return values, nil
}

// single returns the one value of a property that cannot repeat.
func single(values map[string][]string, name string) (string, bool) {
	if len(values[name]) != 1 {
		return "", false
	}
	return values[name][0], true
}

// formatTimespan matches systemd format_timespan with accuracy 0, which
// systemctl show uses for USec properties.
func formatTimespan(t uint64) string {
	if t == 0 {
		return "0"
	}
	units := []struct {
		suffix string
		usec   uint64
	}{{"y", 31557600000000}, {"month", 2629800000000}, {"w", 604800000000}, {"d", 86400000000},
		{"h", 3600000000}, {"min", 60000000}, {"s", 1000000}, {"ms", 1000}, {"us", 1}}
	var parts []string
	for _, unit := range units {
		if t == 0 {
			break
		}
		if t < unit.usec {
			continue
		}
		whole, rest := t/unit.usec, t%unit.usec
		if t < 60000000 && rest > 0 && unit.usec > 1 {
			digits := len(strconv.FormatUint(unit.usec, 10)) - 1
			parts = append(parts, fmt.Sprintf("%d.%0*d%s", whole, digits, rest, unit.suffix))
			break
		}
		parts = append(parts, strconv.FormatUint(whole, 10)+unit.suffix)
		t = rest
	}
	return strings.Join(parts, " ")
}

// limitFile is one cgroup interface file and its exact expected content.
// want is empty for a check that needs only the presence of the file. An
// optional file may be absent: a kernel without CPU burst has no
// cpu.max.burst, and then no burst applies.
type limitFile struct {
	name, want, feature string
	optional            bool
}

// featureSteps gives the next step for a kernel feature that is off.
var featureSteps = map[string]string{
	"swap accounting": "enable swap accounting: the kernel command line must not set `cgroup.memory=noswap` (older kernels need `swapaccount=1`), then reboot",
	"iocost":          "the kernel needs CONFIG_BLK_CGROUP_IOCOST, or remove ioWeight through setup",
}

// missingControlFile reports a control file that an existing group does
// not have, because the kernel feature that gives it is off.
func missingControlFile(group string, file limitFile) error {
	err := fmt.Errorf("%w: %s/%s is missing (%s is off)", ErrLimitInterface, group, file.name, file.feature)
	if step, ok := featureSteps[file.feature]; ok {
		return fmt.Errorf("%w; %s", err, step)
	}
	return err
}

// readLimitFile reads one control file of group. An absent file keeps
// errControlFileAbsent, so the caller can decide. Any other failure is a
// readback failure.
func (m *SystemdManager) readLimitFile(group string, file limitFile) ([]byte, error) {
	data, err := m.cgroups.read(group, file.name)
	if errors.Is(err, errControlFileAbsent) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s: %w", ErrLimitReadback, group, file.name, err)
	}
	return data, nil
}

// expectedLimitFiles gives the cgroup readback of a unit with limits. It
// reads the files that set each control in use, also when the control is
// unset, so a raised default also counts as drift. Other files of a
// controller, such as cpu.idle or memory.low, are not read.
//
// provider is the IOWeight provider: "bfq", "iocost" or empty. As in
// expectedLimitProperties, pids.max needs a set tasksMax.
func expectedLimitFiles(l control.ResourceLimits, provider string) []limitFile {
	files := []limitFile{
		{name: "memory.high", want: formatMiB(l.MemoryHighMiB, "max"), feature: "memory"},
		{name: "memory.max", want: formatMiB(l.MemoryMaxMiB, "max"), feature: "memory"},
		{name: "memory.swap.max", want: formatSwap(l, "max"), feature: "swap accounting"},
		{name: "pids.max", want: strconv.FormatUint(l.TasksMax, 10), feature: "pids"},
	}
	files = append(files, expectedCPUFiles(l)...)
	return append(files, expectedIOFiles(l, provider)...)
}

// expectedCPUFiles uses the systemd defaults for an unset weight or quota.
func expectedCPUFiles(l control.ResourceLimits) []limitFile {
	weight, quota := uint64(100), "max"
	if l.CPUWeight != 0 {
		weight = l.CPUWeight
	}
	if l.CPUQuotaPercent != 0 {
		quota = strconv.FormatUint(l.CPUQuotaPercent*1000, 10)
	}
	files := []limitFile{{name: "cpu.weight", want: strconv.FormatUint(weight, 10), feature: "cpu"}, {name: "cpu.max", want: quota + " 100000", feature: "cpu"}}
	if l.CPUQuotaPercent != 0 {
		// A burst lets the unit go above its quota for a while.
		files = append(files, limitFile{name: "cpu.max.burst", want: "0", feature: "cpu burst", optional: true})
	}
	return files
}

func expectedIOFiles(l control.ResourceLimits, provider string) []limitFile {
	var files []limitFile
	if l.ThrottlesIO() {
		fields := []string{l.IODeviceNumber}
		for _, limit := range ioLimits {
			value := "max"
			if v := limit.value(l); v != 0 {
				value = strconv.FormatUint(v, 10)
			}
			fields = append(fields, limit.key+"="+value)
		}
		files = append(files, limitFile{name: "io.max", want: strings.Join(fields, " "), feature: "I/O throttling"})
	}
	if l.IOWeight != 0 {
		files = append(files, limitFile{name: "io.weight", want: "default " + strconv.FormatUint(l.IOWeight, 10), feature: "iocost"})
		if provider == "bfq" {
			files = append(files, limitFile{name: "io.bfq.weight", want: "default " + strconv.FormatUint(bfqWeight(l.IOWeight), 10), feature: "BFQ"})
		}
	}
	return files
}

// bfqWeight is the systemd mapping of IOWeight 1..10000 to BFQ 1..1000.
func bfqWeight(weight uint64) uint64 {
	if weight <= 100 {
		return weight
	}
	return 100 + (weight-100)*900/9900
}

// readbackLimits compares the cgroup files of a started unit with the
// catalog. provider is the I/O weight provider that checkHostProtection
// found. A missing group is a readback failure; a missing file in an existing
// group with the controller enabled is an interface failure.
func (m *SystemdManager) readbackLimits(group string, l control.ResourceLimits, provider string) error {
	if err := m.requireControllers(group, l, unitControllerRemedy(path.Base(group))); err != nil {
		return err
	}
	for _, file := range expectedLimitFiles(l, provider) {
		data, err := m.readLimitFile(group, file)
		if errors.Is(err, errControlFileAbsent) {
			if file.optional {
				continue
			}
			if _, groupErr := m.cgroups.read(group, "cgroup.controllers"); groupErr != nil {
				// The group went away during the readback.
				return fmt.Errorf("%w: %s/%s: %w", ErrLimitReadback, group, file.name, errors.Join(err, groupErr))
			}
			return missingControlFile(group, file)
		}
		if err != nil {
			return err
		}
		if got := strings.TrimSuffix(string(data), "\n"); got != file.want {
			return fmt.Errorf("%w: %s was %q, not %q", ErrLimitDrift, file.name, got, file.want)
		}
	}
	return nil
}

// verifyStartedLimits runs after start and at each health check, so every
// path that reports a unit with limits healthy repeats the host protection
// checks and the cgroup readback. It is a probe and changes nothing. A unit
// that stopped by itself fails its health check the usual way. Its callers
// run it only for a unit with limits.
func (m *SystemdManager) verifyStartedLimits(ctx context.Context, p control.WorkloadProfile) error {
	_, provider, err := m.checkHostProtection(ctx, p)
	if err != nil {
		return tagStarted(err)
	}
	// The readback reads only files of the unit cgroup, so a failed read
	// there proves no limit of a running unit.
	err = m.readbackLimits(p.Cgroup, p.NativeModel.Owned.Limits, provider)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrLimitReadback) && ctx.Err() == nil {
		return m.classifyStartedReadback(ctx, p, err)
	}
	return tagStarted(err)
}

// classifyStartedReadback decides whether a failed read of a limit file of
// a started unit proves anything. A running unit, or an inactive unit with
// processes left in its cgroup, fails the check. A stopped unit, or a state
// that cannot be read, proves nothing.
func (m *SystemdManager) classifyStartedReadback(ctx context.Context, p control.WorkloadProfile, err error) error {
	state, stateErr := m.unitState(ctx, p.Unit)
	if stateErr != nil {
		// The state is unknown: a later probe can decide.
		return errors.Join(err, stateErr)
	}
	if state.active == "active" {
		// The unit runs, and a limit file cannot be read: no limit is
		// proven, so the check fails.
		return checkFailed(err)
	}
	switch emptyErr := m.cgroups.empty(p.Cgroup); {
	case emptyErr == nil:
		return fmt.Errorf("%s stopped during limit readback: %w", p.Unit, err)
	case errors.Is(emptyErr, errCgroupPopulated):
		// An inactive unit still has processes in its cgroup.
		return checkFailed(err)
	default:
		// The cgroup state is unknown: a later probe can decide.
		return errors.Join(err, emptyErr)
	}
}
