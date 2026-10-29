package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestAdoptedDropInLaunchStartStopIdentityAndDrift(t *testing.T) {
	workingDirectory := t.TempDir()
	identity := "selected"
	server := httptest.NewServer(servingAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, identity)
		}
	}))
	defer server.Close()
	m, runner, p, cmd := nativeFixture(t, "vllm", server.URL)
	original, err := os.ReadFile(p.NativeModel.LaunchFile)
	if err != nil {
		t.Fatal(err)
	}
	command := strings.TrimSpace(strings.Split(string(original), "ExecStart=")[1]) + " --max-model-len 4096"
	model := strings.Fields(command)[2]
	original = append(original, []byte("ExecStartPre=/usr/bin/true\n")...)
	if err := os.WriteFile(p.NativeModel.LaunchFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(original))
	drop := filepath.Join(filepath.Dir(p.NativeModel.LaunchFile), "10-tuning.conf")
	raw := []byte("[Service]\nExecStart=\nExecStart=" + command + "\nExecStartPre=/usr/bin/test -f " + model + "\nWorkingDirectory=" + workingDirectory + "\nLimitMEMLOCK=infinity\n")
	if err := os.WriteFile(drop, raw, 0600); err != nil {
		t.Fatal(err)
	}
	launch, err := inspectAutomaticLaunchSourcesWithValidator(p.NativeModel.LaunchFile, "vllm", []string{drop}, fixtureExecutableValidator)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.DropIns = launch.DropIns
	m.config.Catalog.Profiles[2] = p
	cmd = strings.Replace(cmd, " --property=Environment ", " --property=WorkingDirectory --property=Environment ", 1)
	runner.outputs[cmd] = []byte("Environment=HF_HUB_OFFLINE=1\nEnvironmentFiles=" + p.NativeModel.APIKeyFile + " (ignore_errors=no)\nWorkingDirectory=" + workingDirectory + "\nFragmentPath=" + p.NativeModel.LaunchFile + "\nDropInPaths=" + drop + "\nNeedDaemonReload=no\nExecStart={ path=" + strings.Fields(command)[0] + " ; argv[]=" + command + " ; ignore_errors=no ; }\nExecStartPre={ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; }\nExecStartPre={ path=/usr/bin/test ; argv[]=/usr/bin/test -f " + model + " ; ignore_errors=no ; }\n")
	ctx := context.Background()
	if err := m.Start(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Healthy(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.ReleasedFor(ctx, "text"); err != nil {
		t.Fatal(err)
	}
	identity = "other"
	if m.Healthy(ctx, p.ID) == nil {
		t.Fatal("wrong model identity accepted")
	}
	for path, want := range map[string][]byte{p.NativeModel.LaunchFile: original, drop: raw} {
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, want) {
			t.Fatal("adopted source modified", path)
		}
	}
	if p.NativeModel.LaunchSHA256 != fmt.Sprintf("%x", sha256.Sum256(original)) || !slices.Equal(p.NativeModel.DropIns, launch.DropIns) {
		t.Fatal("source evidence lost")
	}
	runner.outputs[cmd] = []byte(strings.Replace(string(runner.outputs[cmd]), " --max-model-len 4096", " --max-model-len 8192", 1))
	if m.verifyNativeBinding(ctx, p) == nil {
		t.Fatal("loaded main command drift accepted")
	}
	if err := os.WriteFile(drop, append(raw, []byte("# drift\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if verifyNativeLaunchWithValidator(*p.NativeModel, fixtureExecutableValidator) == nil {
		t.Fatal("drop-in hash drift accepted")
	}
}

func TestMalformedExternalConfigurationCannotReachQualification(t *testing.T) {
	good, _ := vllmLaunchFixture(t)
	if _, err := parseExternalLaunchSources([][]byte{[]byte(good)}); err != nil {
		t.Fatal("baseline launch rejected", err)
	}
	for _, raw := range []string{"[Service]\nmalformed", good + "ExecStartPre=/usr/bin/true \\", good + "Description=" + strings.Repeat("x", 1<<20), good + "ExecStart=$COMMAND\n", good + strings.Repeat("ExecStartPre=/usr/bin/true\n", 33)} {
		if _, err := parseExternalLaunchSources([][]byte{[]byte(raw)}); err == nil {
			t.Fatal("malformed or unbounded configuration accepted")
		}
	}
	n := control.NativeModel{Runtime: "vllm", Endpoint: "http://localhost:9000", Owned: &control.OwnedLaunch{}, DropIns: []control.LaunchSource{{Path: "/opt/10-extra.conf", SHA256: strings.Repeat("a", 64)}}}
	if _, err := inspectQualifiedNativeLaunch("/missing", n, fixtureExecutableValidator); err == nil {
		t.Fatal("owned source override accepted during inspection")
	}
	if err := verifyNativeLaunchWithValidator(n, fixtureExecutableValidator); err == nil {
		t.Fatal("owned source override accepted during verification")
	}
}

func TestLoadedSourceBindingsRejectChangedOrderOrPaths(t *testing.T) {
	file := "/opt/worker.service"
	a := control.LaunchSource{Path: "/opt/10-first.conf", SHA256: strings.Repeat("a", 64)}
	b := control.LaunchSource{Path: "/opt/20-second.conf", SHA256: strings.Repeat("b", 64)}
	values := map[string]string{"FragmentPath": file, "DropInPaths": a.Path + " " + b.Path}
	if err := CheckNativeBindingSources(values, "worker.service", file, []control.LaunchSource{a, b}); err != nil {
		t.Fatal(err)
	}
	values["DropInPaths"] = b.Path + " " + a.Path
	if CheckNativeBindingSources(values, "worker.service", file, []control.LaunchSource{a, b}) == nil {
		t.Fatal("changed source order accepted")
	}
	values["DropInPaths"] = ""
	if err := CheckNativeBinding(values, "worker.service", file); err != nil {
		t.Fatal(err)
	}
	values["DropInPaths"] = a.Path
	if CheckNativeBinding(values, "worker.service", file) == nil {
		t.Fatal("owned source override accepted")
	}
	values["FragmentPath"] = "/other"
	if CheckNativeBinding(values, "worker.service", file) == nil {
		t.Fatal("wrong primary source accepted")
	}
	if CheckLoadedLaunchCommand("{ path=/usr/bin/vllm ; argv[]=/usr/bin/vllm serve /models/a ; ignore_errors=yes ; }", "/usr/bin/vllm serve /models/a") == nil {
		t.Fatal("loaded main ignore-errors override accepted")
	}
}

func TestSystemctlRepeatedPreCommandPropertiesRetainExactOrderedBinding(t *testing.T) {
	first := "{ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; start_time=[n/a] ; }"
	second := "{ path=/usr/bin/test ; argv[]=/usr/bin/test -f /opt/model ; ignore_errors=yes ; start_time=[n/a] ; }"
	commands := []string{"/usr/bin/true", "-/usr/bin/test -f /opt/model"}
	props, err := ParseUnitProperties([]byte("Id=worker.service\nExecStartPre=" + first + "\nExecStartPre=" + second + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLoadedPreCommands(props["ExecStartPre"], commands); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{second + " " + first, first, first + " " + strings.Replace(second, "ignore_errors=yes", "ignore_errors=no", 1), first + " " + strings.Replace(second, "/opt/model", "/opt/other", 1)} {
		if CheckLoadedPreCommands(value, commands) == nil {
			t.Fatal("changed ordered startup binding accepted", value)
		}
	}
	for _, raw := range []string{
		"Id=worker.service\nId=other.service\n",
		"ExecStart=one\nExecStart=two\n",
		"DropInPaths=/first\nDropInPaths=/second\n",
		"ExecStartPre=\nExecStartPre=" + first + "\n",
		"ExecStartPre=" + first + "\nExecStartPre=\n",
		"ExecStartPre=" + first + "\nExecStartPre=malformed\n",
		strings.Repeat("ExecStartPre="+first+"\n", 33),
	} {
		if _, err := ParseUnitProperties([]byte(raw)); err == nil {
			t.Fatal("ambiguous or unbounded systemctl metadata accepted", raw)
		}
	}
	if _, err := ParseUnitProperties([]byte(strings.Repeat("ExecStartPre="+first+"\n", 32))); err != nil {
		t.Fatal("bounded systemctl command list rejected", err)
	}
}

func TestExternalStartupPreparationRejectsInterpretersInUnitAndDropIn(t *testing.T) {
	base, n := vllmLaunchFixture(t)
	good := []byte(base + "ExecStartPre=/usr/bin/true --\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.service")
	drop := filepath.Join(dir, "10-preflight.conf")
	for _, inDropIn := range []bool{false, true} {
		for _, command := range []string{"/usr/bin/perl /tmp/preflight.pl", "/usr/bin/python3 /tmp/preflight.py", "/usr/bin/timeout 1 /usr/bin/perl /tmp/preflight.pl"} {
			raw := []byte("[Service]\nExecStartPre=" + command + "\n")
			var dropPaths []string
			primary := append([]byte(nil), good...)
			if inDropIn {
				if err := os.WriteFile(drop, raw, 0600); err != nil {
					t.Fatal(err)
				}
				dropPaths = []string{drop}
			} else {
				primary = append(primary, raw...)
			}
			if err := os.WriteFile(path, primary, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := inspectAutomaticLaunchSourcesWithValidator(path, n.Runtime, dropPaths, func(string) error { return nil }); err == nil {
				t.Errorf("external interpreter accepted (drop-in=%v): %s", inDropIn, command)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, primary) {
				t.Fatal("inspection changed adopted unit")
			}
			if inDropIn {
				after, err = os.ReadFile(drop)
				if err != nil || !bytes.Equal(after, raw) {
					t.Fatal("inspection changed adopted drop-in")
				}
			}
		}
	}
}
