"""Actual packaged entrypoints + user systemd + SQLite, with no NVIDIA hardware.

Runs only inside setup-desktop-integration.sh's disposable OS account. The GPU
probe is an explicit fixture; this does not qualify GNOME or package installation.
"""
import copy
import hashlib
import json
import os
import pathlib
import pwd
import subprocess

HOME = pathlib.Path(pwd.getpwuid(os.geteuid()).pw_dir)
ROOT = HOME / ".config/gpu-workload-supervisor"
UNIT = "gpu-workload-supervisor-reconcile.service"
TIMER = "gpu-workload-supervisor-idle.timer"


def run(*args, request=None, error=None):
    result = subprocess.run(args, input=None if request is None else json.dumps(request) + "\n",
                            text=True, capture_output=True, timeout=40, check=False)
    if error is not None:
        assert result.returncode != 0, f"unexpected success: {args}"
        assert error in result.stderr, (args, result.stderr)
    else:
        assert result.returncode == 0, (args, result.stdout, result.stderr)
    return result.stdout


def setup(action, request=None, error=None):
    return run("/usr/bin/gpu-setup", action, request=request, error=error)


def write(path, data, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.write_text(data)
    path.chmod(mode)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def prepare_session():
    # Hosted images may seed user-manager environments with the runner account.
    # Change only this newly created manager; verify the actual service boundary.
    os.umask(0o077)
    session = {
        "HOME": str(HOME), "USER": pwd.getpwuid(os.geteuid()).pw_name,
        "LOGNAME": pwd.getpwuid(os.geteuid()).pw_name, "PATH": "/usr/bin:/bin",
        "XDG_RUNTIME_DIR": f"/run/user/{os.geteuid()}",
        "DBUS_SESSION_BUS_ADDRESS": f"unix:path=/run/user/{os.geteuid()}/bus",
        "XDG_CONFIG_HOME": str(HOME / ".config"),
        "XDG_DATA_HOME": str(HOME / ".local/share"),
        "XDG_CACHE_HOME": str(HOME / ".cache"),
        "XDG_STATE_HOME": str(HOME / ".local/state"),
        "XDG_DATA_DIRS": "/usr/local/share:/usr/share", "XDG_CONFIG_DIRS": "/etc/xdg",
    }
    run("systemctl", "--user", "set-environment", *[f"{key}={value}" for key, value in session.items()])
    output = run("systemctl", "--user", "show-environment")
    actual = dict(line.split("=", 1) for line in output.splitlines() if "=" in line)
    probe = "import json,os,sys;print(json.dumps({key:os.environ.get(key) for key in sys.argv[1:]}))"
    service = json.loads(run("systemd-run", "--user", "--pipe", "--wait", "--collect", "--quiet",
                             "/usr/bin/python3", "-c", probe, *session))
    for environment in (actual, service):
        for key, expected in session.items():
            assert environment.get(key) == expected, f"disposable session mismatch: {key}"



def main():
    assert os.geteuid() != 0
    assert HOME.name == "home" and HOME.parent.name.startswith("gws-setup-integration.")
    assert not ROOT.exists(), "requires a fresh disposable account"
    prepare_session()
    user_units = HOME / ".config/systemd/user"
    workload = user_units / "gws-ci-workload.service"
    write(workload, "[Service]\nType=exec\nExecStart=/usr/bin/sleep infinity\n")
    for directory in (HOME / ".config", user_units.parent, user_units):
        assert directory.stat().st_mode & 0o022 == 0, f"untrusted fixture directory: {directory}"
    run("systemctl", "--user", "daemon-reload")
    run("systemctl", "--user", "start", workload.name)
    group = run("systemctl", "--user", "show", "--property=ControlGroup", "--value", workload.name).strip()
    assert group.startswith("/user.slice/")
    run("systemctl", "--user", "stop", workload.name)
    probe = pathlib.Path("/usr/bin/gws-ci-nvidia-fixture")
    discovered = json.loads(setup("discover"))
    # Only supported application units are discovery candidates. Generic units
    # remain valid when explicitly configured below.
    assert workload.name not in discovered["units"]
    assert not any(app.get("unit") == workload.name for app in discovered["applications"])
    request = discovered["request"]
    request["profile"]["nvidiaSMIPath"] = str(probe)
    request["catalog"]["profiles"] = [{"id": "ci-workload", "label": "CI workload",
        "adapter": "systemd", "unit": workload.name, "cgroup": group,
        "healthURL": "http://127.0.0.1:9/health", "bootPolicy": "stop-to-idle"}]
    preview = json.loads(setup("validate", request))
    assert preview["catalog"] == request["catalog"]
    assert not ROOT.exists(), "preview mutated configuration"
    setup("apply", request, "explicitly confirm")
    request["confirmQuiesced"] = True
    # A real filesystem conflict fails after catalog commit. Discovery must retain
    # the exact durable request and resume forward once the conflict is removed.
    conflict = user_units / UNIT
    write(conflict, "# user-owned override must not be replaced\n")
    setup("apply", request, "user reconciliation unit exists")
    assert conflict.read_text() == "# user-owned override must not be replaced\n"
    state = pathlib.Path(request["profile"]["statePath"])
    marker = pathlib.Path(str(state) + ".deployment.json")
    assert json.loads(marker.read_text())["maintenance"]
    pending = json.loads(setup("discover"))
    assert pending["pending"] and pending["request"] == request
    setup("reconcile", error="maintenance")
    conflict.unlink()
    setup("apply", pending["request"])
    assert not json.loads(marker.read_text())["maintenance"]
    link = user_units / "default.target.wants" / UNIT
    assert link.is_symlink() and os.readlink(link) == "/usr/lib/systemd/user/" + UNIT
    timer_link = user_units / "timers.target.wants" / TIMER
    assert timer_link.is_symlink() and os.readlink(timer_link) == "/usr/lib/systemd/user/" + TIMER
    assert run("systemctl", "--user", "show", "--property=ActiveState", "--value", UNIT).strip() == "inactive"
    run("systemctl", "--user", "start", UNIT)
    assert run("systemctl", "--user", "show", "--property=Result", "--value", UNIT).strip() == "success"
    assert run("systemctl", "--user", "show", "--property=ActiveState", "--value", workload.name).strip() == "inactive"
    # Another catalog commit invalidates a previously reviewed request.
    before = json.loads(run("/usr/bin/gpu-operator", request={
        "protocolVersion": 1, "requestId": "before-update", "action": "status"}))
    assert before["code"] == "ok" and before["requestId"] == "before-update", before
    assert before["status"]["activeWorkload"] == "idle"
    assert before["status"]["workloads"] == [{"id": "idle", "label": "Idle"}, {"id": "ci-workload", "label": "CI workload"}]
    configured = json.loads(setup("discover"))
    assert configured["request"]["catalog"] == request["catalog"]
    assert not any(app.get("unit") == workload.name for app in configured["applications"])
    current = configured["request"]
    current["confirmQuiesced"] = True
    updated = copy.deepcopy(current)
    updated["catalog"]["profiles"][0]["label"] = "Updated workload"
    setup("apply", updated)
    rejected = json.loads(run("/usr/bin/gpu-operator", request={
        "protocolVersion": 1, "requestId": "old-preview", "action": "take-control",
        "expected": before["status"]["expected"]}))
    assert rejected["code"] == "stale_state" and rejected["requestId"] == "old-preview", rejected
    after = json.loads(run("/usr/bin/gpu-operator", request={
        "protocolVersion": 1, "requestId": "after-update", "action": "status"}))
    assert after["code"] == "ok", after
    assert after["status"]["owner"] == before["status"]["owner"]
    assert after["status"]["workloads"] == [{"id": "idle", "label": "Idle"}, {"id": "ci-workload", "label": "Updated workload"}]
    profile_hash = digest(ROOT / "operator.json")
    setup("apply", current, "configuration revision changed")
    assert digest(ROOT / "operator.json") == profile_hash
    assert not json.loads(marker.read_text())["maintenance"]
    assert json.loads(setup("discover"))["request"]["catalog"] == updated["catalog"]
    backups = list((ROOT / "backups").glob("activation-*"))
    assert backups, "managed reactivation must retain its rollback tuple"
    complete = [path for path in backups if (path / "manifest.json").exists()]
    assert complete, "missing previous activated binary tuple"
    for backup in complete:
        manifest = json.loads((backup / "manifest.json").read_text())
        assert set(manifest["hashes"]) == {"gpu-mode", "gpu-workload-proxy", "gpu-operator", "gpu-setup"}
        profile = json.loads((backup / "operator.json").read_text())
        assert manifest["release"] == profile["activatedRelease"]
        for name, expected in manifest["hashes"].items():
            assert digest(backup / name) == expected
        for name in ("operator.json", "catalog.json", "ownership.json", "state.db", "state.db.deployment.json"):
            assert (backup / name).is_file(), name
    preserved = {path: digest(path) for path in (ROOT / "operator.json", state, workload)}
    setup("remove-integration")
    assert not link.exists() and not link.is_symlink()
    assert not timer_link.exists() and not timer_link.is_symlink()
    for path, expected in preserved.items():
        assert digest(path) == expected, f"removal changed {path}"
    # A later activation recreates only owned integration and preserves the unit.
    request = json.loads(setup("discover"))["request"]
    request["confirmQuiesced"] = True
    setup("apply", request)
    assert link.is_symlink()
    assert timer_link.is_symlink()
    assert digest(workload) == preserved[workload]
    print("PASS: packaged setup, real user systemd, interrupted resume, stale preview, backups, removal/reapply")


if __name__ == "__main__":
    main()
