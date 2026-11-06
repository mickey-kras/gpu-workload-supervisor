package runtime

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// requiredControllers lists the cgroup controllers that apply the configured
// controls, in the order of the cgroup.controllers file.
func requiredControllers(l control.ResourceLimits) []string {
	var controllers []string
	if l.UsesCPU() {
		controllers = append(controllers, "cpu")
	}
	if l.UsesIO() {
		controllers = append(controllers, "io")
	}
	if l.UsesMemory() {
		controllers = append(controllers, "memory")
	}
	if l.TasksMax != 0 {
		controllers = append(controllers, "pids")
	}
	return controllers
}

// requireControllers reads cgroup.controllers of group and names the first
// required controller that is absent. remedy gives the next step for that
// controller.
func (m *SystemdManager) requireControllers(group string, l control.ResourceLimits, remedy func(controller string) string) error {
	data, err := m.cgroups.read(group, "cgroup.controllers")
	if err != nil {
		return fmt.Errorf("%w: %s/cgroup.controllers: %w", ErrLimitReadback, group, err)
	}
	enabled := strings.Fields(string(data))
	for _, controller := range requiredControllers(l) {
		if !slices.Contains(enabled, controller) {
			return fmt.Errorf("%w: %s is absent from %s/cgroup.controllers; %s", ErrLimitController, controller, group, remedy(controller))
		}
	}
	return nil
}

// managerControllerRemedy is the next step for a controller that the user
// manager root lacks: the delegation of user@.service. The only admin
// drop-in that the host cap rules accept on the user manager is the
// delegation drop-in, so the step names it.
func (m *SystemdManager) managerControllerRemedy(controller string) string {
	restart := ", run `sudo systemctl daemon-reload`, then restart " + m.host.managerUnit() + " from a verified Idle"
	if controller == "io" {
		return "check the Delegate=io drop-in of the host protection step. After you add or change it" + restart
	}
	return "add " + controller + " to Delegate= in " + m.host.delegateDropIn() + restart
}

// unitControllerRemedy is the next step for a controller that the unit
// cgroup lacks. The host protection checks found it at the user manager
// root, so delegation is not the cause. The supervisor manages the unit,
// so the unit starts again through gpu-mode, not systemctl.
func unitControllerRemedy(unit string) func(string) string {
	return func(string) string {
		return "the user manager root has it, so systemd did not enable it for " + unit +
			": run `systemctl --user daemon-reload`, then start the workload again through gpu-mode"
	}
}
