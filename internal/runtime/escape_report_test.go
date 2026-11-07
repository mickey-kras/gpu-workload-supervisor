package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Exactly maxEscapeRoutes findings need no "more" entry. One more finding
// leaves two out.
func TestEscapeRouteReportBoundary(t *testing.T) {
	report := func(n int) []EscapeRoute {
		var r routeReport
		for i := range n {
			r.add("cron", "job"+strconv.Itoa(i))
		}
		return r.bounded()
	}
	if routes := report(maxEscapeRoutes); len(routes) != maxEscapeRoutes || routes[len(routes)-1].Route == "more" {
		t.Fatalf("report of %d findings = %+v", maxEscapeRoutes, routes)
	}
	if routes := report(maxEscapeRoutes + 1); len(routes) != maxEscapeRoutes || routes[len(routes)-1] != (EscapeRoute{Route: "more", Item: "2 more entries (cron)", Count: 2}) {
		t.Fatalf("report of %d findings = %+v", maxEscapeRoutes+1, routes)
	}
}

// The incomplete entry stays in the report, also when "cannot check"
// items fill it.
func TestIncompleteEntryStaysInTheReport(t *testing.T) {
	var r routeReport
	for i := range 40 {
		r.unchecked("cron", "/etc/cron.d/job"+strconv.Itoa(i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	escapeScanner{ctx: ctx}.markStopped(&r)
	routes := r.bounded()
	want := EscapeRoute{Route: "more", Item: "10 more entries, 10 of them cannot check (cron)", Unchecked: true, Count: 10, UncheckedCount: 10}
	if len(routes) != maxEscapeRoutes || routes[0].Route != "incomplete" || routes[len(routes)-1] != want {
		t.Fatalf("report = %+v", routes)
	}
}

// A context that ends after the last check does not mark a finished scan
// as incomplete.
func TestContextEndsAfterTheScan(t *testing.T) {
	f := newEscapeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.s.run = func(context.Context, string, ...string) ([]byte, error) {
		cancel()
		return []byte("Id=job.service\nUser=alice\n"), nil
	}
	routes := f.s.scan(ctx)
	if len(routes) != 1 || routes[0] != (EscapeRoute{Route: "system-units", Item: "job.service runs as alice"}) {
		t.Fatalf("report = %+v", routes)
	}
}

// A killed run can end in a cut line. "User=12" can be the start of
// "User=123", so it gives no finding for uid 12.
func TestCanceledRunIgnoresTheCutLine(t *testing.T) {
	f := newEscapeFixture(t)
	f.s.host.uid = 12
	f.write(t, "/etc/passwd", "alice:x:12:5000::/home/alice:/bin/sh\n")
	ctx, cancel := context.WithCancel(context.Background())
	f.s.run = func(context.Context, string, ...string) ([]byte, error) {
		cancel()
		return []byte("Id=a.service\nUser=bob\n\nId=b.service\nUser=12"), errors.New("signal: killed")
	}
	routes := f.s.scan(ctx)
	if len(routes) != 1 || routes[0].Route != "incomplete" {
		t.Fatalf("report = %+v", routes)
	}
}

// systemctl prints User before Id. A block that a failed run cuts between
// User and Id gives no finding without a unit name.
func TestUnitBlocksInAnyOrder(t *testing.T) {
	f := newEscapeFixture(t)
	f.r.outputs[unitShow] = []byte("User=alice\nId=job.service\n\nUser=\nId=sshd.service\n")
	f.requireReport(t, "system-units: job.service runs as alice")
	f.r.outputs[unitShow] = []byte("User=bob\nId=a.service\n\nUser=alice\n")
	f.r.errs[unitShow] = errors.New("exit status 1")
	lines := f.report()
	if len(lines) != 1 || lines[0] != "system-units: cannot check the loaded system units (systemctl show: exit status 1)" {
		t.Fatalf("escape routes = %q", lines)
	}
}

// The context ends while the scan reads the first file of /etc/cron.d or
// of a polkit directory. The scan reads no later file.
func TestContextEndsBetweenFiles(t *testing.T) {
	for name, dir := range map[string]string{"cron.d": "/etc/cron.d", "polkit": "/etc/polkit-1/rules.d"} {
		t.Run(name, func(t *testing.T) {
			f := newEscapeFixture(t)
			files := map[uint64]bool{}
			for i := range 3 {
				file := dir + "/job" + strconv.Itoa(i) + ".rules"
				f.write(t, file, "* * * * * alice org.freedesktop.policykit.exec\n")
				files[f.inode(t, file)] = true
			}
			ctx, cancel := context.WithCancel(context.Background())
			read := 0
			f.s.probe.verify = func(fd int) error {
				var stat unix.Stat_t
				if err := unix.Fstat(fd, &stat); err != nil {
					return err
				}
				if files[stat.Ino] {
					read++
					cancel()
				}
				return nil
			}
			routes := f.s.scan(ctx)
			if read != 1 || routes[0].Route != "incomplete" {
				t.Fatalf("read %d files: %+v", read, routes)
			}
		})
	}
}

// When the context ends during systemctl show, the report says that the
// scan is incomplete, not that the command failed.
func TestContextEndsDuringSystemctl(t *testing.T) {
	f := newEscapeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.s.run = func(context.Context, string, ...string) ([]byte, error) {
		cancel()
		return nil, errors.New("signal: killed")
	}
	routes := f.s.scan(ctx)
	if len(routes) != 1 || routes[0] != (EscapeRoute{Route: "incomplete", Item: "cannot check the rest of the routes (context canceled)", Unchecked: true}) {
		t.Fatalf("report = %+v", routes)
	}
}

// Output over the runner limit gives a clear reason. The complete blocks
// of the output still give findings. The cut last line names the account
// on its own, but it can be the start of a longer value, so it gives none.
func TestSystemctlOutputLimit(t *testing.T) {
	f := newEscapeFixture(t)
	tail := "Id=cut.service\nUser=alice"
	output := "Id=job.service\nUser=alice\n\n"
	block := "Id=x.service\nUser=bob\n\n"
	for len(output)+len(block)+len(tail)+16 < CommandOutputLimit {
		output += block
	}
	pad := CommandOutputLimit - len(output) - len(tail) - len("Pad=\n\n")
	output += "Pad=" + strings.Repeat("x", pad) + "\n\n" + tail
	if len(output) != CommandOutputLimit {
		t.Fatalf("output has %d bytes", len(output))
	}
	f.r.outputs[unitShow] = []byte(output)
	f.r.errs[unitShow] = errors.New("signal: broken pipe")
	lines := f.report()
	if len(lines) != 2 || lines[0] != "system-units: cannot check the loaded system units (systemctl show: output exceeds 1 MiB)" ||
		lines[1] != "system-units: job.service runs as alice" {
		t.Fatalf("escape routes = %q", lines)
	}
}

// The JSON report marks the "cannot check" items and counts the left-out
// findings.
func TestEscapeRouteJSON(t *testing.T) {
	var r routeReport
	r.unchecked("cron", "/etc/cron.deny (permission denied)")
	r.add("at", "/etc/at.deny does not deny alice")
	data, err := json.Marshal(r.bounded())
	if err != nil || string(data) != `[{"route":"cron","item":"cannot check /etc/cron.deny (permission denied)","unchecked":true},{"route":"at","item":"/etc/at.deny does not deny alice"}]` {
		t.Fatalf("JSON = %s, %v", data, err)
	}
	more, err := json.Marshal(EscapeRoute{Route: "more", Item: "2 more entries, 1 of them cannot check (cron)", Unchecked: true, Count: 2, UncheckedCount: 1})
	if err != nil || string(more) != `{"route":"more","item":"2 more entries, 1 of them cannot check (cron)","unchecked":true,"count":2,"uncheckedCount":1}` {
		t.Fatalf("JSON = %s, %v", more, err)
	}
}
