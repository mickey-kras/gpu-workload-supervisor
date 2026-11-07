package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
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
	if routes := report(maxEscapeRoutes + 1); len(routes) != maxEscapeRoutes || routes[len(routes)-1] != (EscapeRoute{Route: "more", Item: "2 more findings (cron)", Count: 2}) {
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
	escapeScanner{ctx: ctx}.stopped(&r)
	routes := r.bounded()
	if len(routes) != maxEscapeRoutes || routes[0].Route != "incomplete" || routes[len(routes)-1].Count != 10 {
		t.Fatalf("report = %+v", routes)
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

// Output over the runner limit gives a clear reason. The complete lines
// of the output still give findings, and a cut last line gives none.
func TestSystemctlOutputLimit(t *testing.T) {
	f := newEscapeFixture(t)
	head := "Id=job.service\nUser=alice\n\n"
	filler := strings.Repeat("Id=x.service\nUser=bob\n\n", commandOutputLimit/len("Id=x.service\nUser=bob\n\n")+1)
	output := (head + filler)[:commandOutputLimit-len("Id=cut.service\nUser=ali")]
	output = output[:strings.LastIndex(output, "\n")+1] + "Id=cut.service\nUser=ali"
	output += strings.Repeat("e", commandOutputLimit-len(output))
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
	more, err := json.Marshal(EscapeRoute{Route: "more", Item: "2 more findings (cron)", Count: 2})
	if err != nil || string(more) != `{"route":"more","item":"2 more findings (cron)","count":2}` {
		t.Fatalf("JSON = %s, %v", more, err)
	}
}
