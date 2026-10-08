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
	"strings"
	"syscall"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestExternalLaunchAppliesResetAppendAndScalarOverrides(t *testing.T) {
	sources := [][]byte{
		[]byte("[Unit]\nDescription=Old\n[Service]\nType=forking\nRestart=always\nExecStart=/bin/sh -c unsupported\nExecStartPre=/usr/bin/false\nEnvironment=OLLAMA_NO_CLOUD=0\nEnvironment=OLLAMA_HOST=127.0.0.1:1\n"),
		[]byte("[Service]\nType=exec\nRestart=no\nExecStart=\nExecStart=/usr/bin/ollama serve\nExecStartPre=\nExecStartPre=/usr/bin/true\nEnvironment=\nEnvironment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=127.0.0.1:8080\nWorkingDirectory=/opt/models\nLimitNOFILE=65536\nKillMode=control-group\n"),
		[]byte("[Service]\nExecStartPre=-/usr/bin/test \\\n# skipped continuation comment\n-f /opt/models/a.gguf\nEnvironment=OLLAMA_HOST=127.0.0.1:9000\nTimeoutStartSec=120\n"),
	}
	unit, err := parseExternalLaunchSources(sources, "ollama")
	if err != nil {
		t.Fatal(err)
	}
	if unit.execStart != "/usr/bin/ollama serve" || unit.host != "127.0.0.1:9000" || !unit.cloudOff || len(unit.preCommands) != 2 || strings.Join(strings.Fields(unit.preCommands[1]), " ") != "-/usr/bin/test -f /opt/models/a.gguf" {
		t.Fatalf("incorrect effective configuration: %+v", unit)
	}
	for _, bad := range []string{"ExecStart=/usr/bin/ollama serve", "ExecStartPost=/usr/bin/true", "EnvironmentFile=/tmp/env", "KillMode=process", "Restart=always", "Type=forking", "UnknownDirective=value"} {
		if _, err := parseExternalLaunchSources(append(sources, []byte("[Service]\n"+bad+"\n")), "ollama"); err == nil || !strings.Contains(err.Error(), strings.Split(bad, "=")[0]) {
			t.Errorf("missing actionable refusal for %s: %v", bad, err)
		}
	}
}

func TestExternalLaunchSourcesRejectUntrustedOrAmbiguousFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.service")
	drop := filepath.Join(dir, "10-options.conf")
	raw := nativeLaunchFixture(t, "llama.cpp", "http://localhost:9000", "selected")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(drop, []byte("[Service]\nLimitMEMLOCK=infinity\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAutomaticLaunchSourcesWithValidator(path, "llama.cpp", []string{drop}, fixtureExecutableValidator); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0620, 0602} {
		os.Chmod(drop, mode)
		if _, err := inspectAutomaticLaunchSourcesWithValidator(path, "llama.cpp", []string{drop}, fixtureExecutableValidator); err == nil {
			t.Fatal("writable source accepted")
		}
	}
	os.Chmod(drop, 0600)
	alias := filepath.Join(dir, "20-alias.conf")
	os.Symlink(drop, alias)
	for _, paths := range [][]string{{alias}, {drop, drop}, {alias, drop}} {
		if _, err := inspectAutomaticLaunchSourcesWithValidator(path, "llama.cpp", paths, fixtureExecutableValidator); err == nil {
			t.Fatal("ambiguous source list accepted", paths)
		}
	}
}

func TestAdoptedDropInLaunchStartStopIdentityAndDrift(t *testing.T) {
	identity := "selected"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, identity)
		}
	}))
	defer server.Close()
	m, runner, p, cmd := nativeFixture(t, "llama.cpp", server.URL)
	original, err := os.ReadFile(p.NativeModel.LaunchFile)
	if err != nil {
		t.Fatal(err)
	}
	command := strings.TrimSpace(strings.Split(string(original), "ExecStart=")[1]) + " --parallel 2 --cont-batching --flash-attn on --spec-type draft-mtp --spec-draft-n-max 4"
	model := strings.Fields(command)[2]
	original = append(original, []byte("ExecStartPre=/usr/bin/true\n")...)
	if err := os.WriteFile(p.NativeModel.LaunchFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(original))
	drop := filepath.Join(filepath.Dir(p.NativeModel.LaunchFile), "10-tuning.conf")
	raw := []byte("[Service]\nExecStart=\nExecStart=" + command + "\nExecStartPre=/usr/bin/test -f " + model + "\nWorkingDirectory=/opt\nLimitMEMLOCK=infinity\n")
	if err := os.WriteFile(drop, raw, 0600); err != nil {
		t.Fatal(err)
	}
	launch, err := inspectAutomaticLaunchSourcesWithValidator(p.NativeModel.LaunchFile, "llama.cpp", []string{drop}, fixtureExecutableValidator)
	if err != nil {
		t.Fatal(err)
	}
	p.NativeModel.DropIns = launch.DropIns
	m.config.Catalog.Profiles[2] = p
	runner.outputs[cmd] = []byte("FragmentPath=" + p.NativeModel.LaunchFile + "\nDropInPaths=" + drop + "\nNeedDaemonReload=no\nExecStart={ path=" + strings.Fields(command)[0] + " ; argv[]=" + command + " ; ignore_errors=no ; }\nExecStartPre={ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; }\nExecStartPre={ path=/usr/bin/test ; argv[]=/usr/bin/test -f " + model + " ; ignore_errors=no ; }\n")
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
	if p.NativeModel.LaunchSHA256 != fmt.Sprintf("%x", sha256.Sum256(original)) || !control.EqualLaunchSources(p.NativeModel.DropIns, launch.DropIns) {
		t.Fatal("source evidence lost")
	}
	runner.outputs[cmd] = []byte(strings.Replace(string(runner.outputs[cmd]), " --parallel 2", " --parallel 3", 1))
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

func TestAdoptionPreservesCompatibleDirectivesAndRejectsUnsafeAlternatives(t *testing.T) {
	n := control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://localhost:9000", Model: "selected"}
	good := string(nativeLaunchFixture(t, n.Runtime, n.Endpoint, n.Model))
	path := filepath.Join(t.TempDir(), "worker.service")
	cases := []struct {
		directive string
		valid     bool
	}{
		{"[Unit]\nDescription=A server\nDocumentation=https://example.invalid/help\nAfter=network.target another.service\nBefore=shutdown.target\n[Install]\nWantedBy=default.target", true},
		{"[Unit]\nAfter=bad@%i.service", false},
		{"[Service]\nWorkingDirectory=/opt/models", true}, {"[Service]\nWorkingDirectory=relative", false},
		{"[Service]\nTimeoutStartSec=120\nTimeoutStopSec=30\nRestartSec=5", true}, {"[Service]\nTimeoutStartSec=infinity", false}, {"[Service]\nTimeoutStopSec=0", false},
		{"[Service]\nLimitNOFILE=65536\nLimitMEMLOCK=infinity", true}, {"[Service]\nLimitNOFILE=0", false}, {"[Service]\nLimitMEMLOCK=bad", false},
		{"[Service]\nStandardOutput=journal\nStandardError=inherit", true}, {"[Service]\nStandardOutput=null\nStandardError=null", true}, {"[Service]\nStandardOutput=file:/tmp/log", false},
		{"[Service]\nKillMode=mixed\nRemainAfterExit=no\nSlice=app.slice", true}, {"[Service]\nRemainAfterExit=yes", false}, {"[Service]\nSlice=foreign.slice", false},
		{"[Service]\nEnvironment=LD_PRELOAD=/tmp/lib.so", false}, {"[Service]\nEnvironment=no-assignment", false}, {"[Service]\nEnvironment=MODEL=$UNTRUSTED", false},
		{"[Service]\nRestart=$RESTART", false}, {"NoSection=value", false},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.directive, "\n", ";"), func(t *testing.T) {
			raw := []byte(tc.directive + "\n" + good)
			// Put service directives after the launch so their section is authoritative.
			if strings.HasPrefix(tc.directive, "[Service]") {
				raw = []byte(good + tc.directive + "\n")
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			launch, err := inspectAutomaticLaunchSourcesWithValidator(path, n.Runtime, nil, fixtureExecutableValidator)
			if (err == nil) != tc.valid {
				t.Fatalf("adoption valid=%v: %v", tc.valid, err)
			}
			if tc.valid && (launch.Model != n.Model || launch.Endpoint != n.Endpoint || launch.SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw))) {
				t.Fatal("preserved directives changed bound identity or evidence")
			}
		})
	}
	if err := qualifyFixtureLaunch([]byte(good+"WorkingDirectory=/opt/models\n"), n); err == nil {
		t.Fatal("owned grammar silently expanded to external directives")
	}
}

func TestLaunchSourceTrustRejectsSpecialMissingAndReplaceablePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.service")
	write := func() {
		t.Helper()
		if err := os.WriteFile(path, []byte("unit"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	for _, candidate := range []string{"relative.service", dir + "/../worker.service", filepath.Join(dir, "missing.service"), dir} {
		if _, err := readLaunchSource(candidate); err == nil {
			t.Fatal("unsafe path accepted", candidate)
		}
	}
	os.Chmod(path, 0000)
	if _, err := readLaunchSource(path); err == nil {
		t.Fatal("unreadable source accepted")
	}
	os.Chmod(path, 0600)
	if err := os.Truncate(path, 1<<20+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchSource(path); err == nil {
		t.Fatal("oversized source accepted")
	}
	os.Remove(path)
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchSource(path); err == nil {
		t.Fatal("FIFO source accepted")
	}
	os.Remove(path)
	write()
	link := filepath.Join(dir, "alias")
	os.Symlink(dir, link)
	if _, err := readLaunchSource(filepath.Join(link, "worker.service")); err == nil {
		t.Fatal("replaceable symlink ancestor accepted")
	}
	os.Chmod(dir, 0777)
	if _, err := readLaunchSource(path); err == nil {
		t.Fatal("writable parent accepted")
	}
	os.Chmod(dir, 0700)
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 1, -1); err != nil {
			t.Logf("filesystem cannot exercise foreign ownership: %v", err)
		} else {
			if _, err := readLaunchSource(path); err == nil {
				t.Fatal("foreign-owned source accepted")
			}
			os.Chown(path, 0, -1)
		}
	}
	if _, err := readLaunchSources(path, []control.LaunchSource{{Path: "relative.conf", SHA256: "bad"}}); err == nil {
		t.Fatal("invalid recorded source accepted")
	}
	if _, err := readLaunchSources(path+"missing", nil); err == nil {
		t.Fatal("missing primary source accepted")
	}
}

func TestMalformedExternalConfigurationCannotReachQualification(t *testing.T) {
	good := "[Service]\nExecStart=/usr/bin/ollama serve\nEnvironment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=localhost:9000\n"
	for _, raw := range []string{"[Service]\nmalformed", good + "ExecStartPre=/usr/bin/true \\", good + "Description=" + strings.Repeat("x", 1<<20), good + "ExecStart=$COMMAND\n", good + strings.Repeat("ExecStartPre=/usr/bin/true\n", 33)} {
		if _, err := parseExternalLaunchSources([][]byte{[]byte(raw)}, "ollama"); err == nil {
			t.Fatal("malformed or unbounded configuration accepted")
		}
	}
	n := control.NativeModel{Runtime: "ollama", Endpoint: "http://localhost:9000", Owned: &control.OwnedLaunch{}, DropIns: []control.LaunchSource{{Path: "/opt/10-extra.conf", SHA256: strings.Repeat("a", 64)}}}
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
	if CheckLoadedLaunchCommand("{ path=/usr/bin/ollama ; argv[]=/usr/bin/ollama serve ; ignore_errors=yes ; }", "/usr/bin/ollama serve") == nil {
		t.Fatal("loaded main ignore-errors override accepted")
	}
}

func TestSystemctlRepeatedPreCommandPropertiesRetainExactOrderedBinding(t *testing.T) {
	first := "{ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; start_time=[n/a] ; }"
	second := "{ path=/usr/bin/test ; argv[]=/usr/bin/test -f /opt/model.gguf ; ignore_errors=yes ; start_time=[n/a] ; }"
	commands := []string{"/usr/bin/true", "-/usr/bin/test -f /opt/model.gguf"}
	props, err := ParseUnitProperties([]byte("Id=worker.service\nExecStartPre=" + first + "\nExecStartPre=" + second + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLoadedPreCommands(props["ExecStartPre"], commands); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{second + " " + first, first, first + " " + strings.Replace(second, "ignore_errors=yes", "ignore_errors=no", 1), first + " " + strings.Replace(second, "/opt/model.gguf", "/opt/other.gguf", 1)} {
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
