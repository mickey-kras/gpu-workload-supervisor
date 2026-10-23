"""Fail closed on every dependency deviation; do not rewrite upstream metadata.

Also check the training runtime for one target and print a bounded JSON report.
The report covers Python and its standard library, the installed distributions
against the lock, native library loading, and Torch and CUDA compatibility.
Run it as the runner does: bin/python3 -I -S -B. It then adds only the runtime
site-packages, so packaging is imported inside the functions that use it.
"""
import sys

# Never write bytecode into the inventoried runtime root, whatever the flags.
sys.dont_write_bytecode = True

import argparse
import base64
from collections import deque
import csv
import hashlib
import importlib.metadata as metadata
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile

# Exact requirements from the unchanged source commit. Absence is intentional only
# for these optional integrations; python-dotenv is replaced by a patched version.
OMITTED = {
    "trackio": "==0.40.0", "gradio": "==6.29.1", "s3fs": "==2025.10.0",
    "gcsfs": "==2025.10.0", "adlfs": "==2026.2.0", "ocifs": "==1.3.2",
    "lm-eval": "==0.4.11", "openenv-core": ">=0.1.0",
}
EXPECTED = {("axolotl", name, spec, "missing") for name, spec in OMITTED.items()}
EXPECTED.add(("axolotl", "python-dotenv", "==1.0.1", "version"))


def deviations(distributions):
    from packaging.requirements import Requirement

    installed = {}
    for distribution in distributions:
        name = canonical(distribution.metadata["Name"])
        if name in installed:
            raise ValueError("duplicate distribution: " + name)
        installed[name] = distribution
    requirements = {
        name: [Requirement(text) for text in distribution.requires or []]
        for name, distribution in installed.items()
    }
    # Every installed distribution activates its base requirements. Requested
    # extras propagate across dependency edges, including cycles, until no new
    # (distribution, extra) context remains. Unrequested extras stay inactive.
    pending = deque((name, "") for name in installed)
    visited = set()
    result = set()
    while pending:
        parent, extra = pending.popleft()
        if (parent, extra) in visited:
            continue
        visited.add((parent, extra))
        context_deviations, requested_extras = context_dependencies(parent, extra, requirements[parent], installed)
        result.update(context_deviations)
        pending.extend(requested_extras)
    return result


def context_dependencies(parent, extra, requirements, installed):
    result = set()
    requested_extras = []
    for requirement in requirements:
        if requirement.marker and not requirement.marker.evaluate({"extra": extra}):
            continue
        name = canonical(requirement.name)
        dependency = installed.get(name)
        if dependency is None:
            result.add((parent, name, str(requirement.specifier), "missing"))
        elif requirement.url or not requirement.specifier.contains(dependency.version, prereleases=True):
            result.add((parent, name, str(requirement.specifier), "version"))
        if dependency is not None:
            requested_extras.extend((name, canonical(requested)) for requested in requirement.extras)
    return result, requested_extras


HERE = Path(__file__).resolve().parent
LOCK = HERE / "requirements.txt"
ASSETS = HERE.parent / "assets.json"
WORKER = HERE.parents[1] / "internal/llmtraining/worker.py"
PROBE = HERE / "runtime_probe.py"
# requirements.in names the interpreter that the lock resolves for.
LOCK_TARGET = "CPython 3.12.14"
QUANTIZED = {"representative-qlora"}
# Only the Axolotl source is installed outside the hash-locked wheel set.
UNLOCKED = {"axolotl"}
STARTUP_MODULES = {"sitecustomize", "usercustomize"}
PROBE_TIMEOUT = 300
KILL_TIMEOUT = 10
OUTPUT_LIMIT = 64 << 10
PROBLEM_LIMIT = 20
PROBLEM_LENGTH = 300
FACT_LENGTH = 64
FACT_ITEMS = 64
# The same rule as ValidGPUUUID in internal/control/native_model.go.
GPU_UUID = re.compile(r"GPU-[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}")
CHECKER_BUG = "report this as a checker bug"
STABLE_KEYS = ("accelerator", "torch", "cuda", "driverCuda", "archList", "devices")


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def load_presets(path=None):
    presets = json.loads((path or ASSETS).read_text(encoding="utf-8"))
    if not isinstance(presets, dict) or not presets or not all(isinstance(name, str) for name in presets):
        raise ValueError("the asset table has no presets")
    return tuple(presets)


def mask(text):
    # Report no host paths: hide the known roots literally, then any other absolute path.
    literals = {
        os.environ.get("HOME", ""): "<home>", tempfile.gettempdir(): "<tmp>", os.getcwd(): "<cwd>",
        sys.base_prefix: "<python>", sys.prefix: "<python>", str(runtime_root()): "<runtime>",
    }
    for literal in sorted(literals, key=len, reverse=True):
        if len(literal) > 1:
            text = text.replace(literal, literals[literal])
    text = re.sub(r"(?<![\w.<>-])/[^\s;:,()'\"]+", "<path>", text)
    return text[:PROBLEM_LENGTH]


def runtime_root():
    # The runner starts <root>/bin/python3; with -S, sys.prefix does not show a virtual environment.
    return Path(sys.executable).parent.parent


def layout_problems(root, stdlib, site):
    # The runner executes bin/python3 from one inventoried root without symlinks.
    problems = []
    if (root / "pyvenv.cfg").exists():
        problems.append("the runtime is a virtual environment; the runner needs a self-contained CPython root")
    executable = root / "bin" / "python3"
    if executable.is_symlink() or not executable.is_file():
        problems.append("bin/python3 is not a regular file; copy the interpreter into the runtime root")
    if site.is_symlink() or not site.is_dir() or not site.is_relative_to(root):
        problems.append("the runtime site-packages is not a regular directory in the runtime root")
    if not stdlib.is_relative_to(root):
        problems.append("the standard library is outside the runtime root; install it into the same root")
    return problems


def module_name(entry):
    # Return the importable top-level name of a directory entry, or None for other files.
    if entry.is_dir():
        skipped = "." in entry.name or entry.name in {"__pycache__", "site-packages", "dist-packages"}
        return None if skipped else entry.name
    # Extension modules carry a platform tag, such as _ssl.cpython-312-x86_64-linux-gnu.so.
    if entry.name.endswith(".so"):
        return entry.name.split(".")[0] or None
    stem, suffix = os.path.splitext(entry.name)
    return stem or None if suffix in {".py", ".pyc"} else None


def stdlib_names(directories):
    names = set(sys.stdlib_module_names)
    for directory in directories:
        if directory.is_dir():
            names.update(name for name in map(module_name, directory.iterdir()) if name is not None)
    return names


def owned_startup_files(distributions, locked):
    # Map each top-level .pth file of a locked wheel to its RECORD hash.
    owned = {}
    for distribution in distributions:
        if canonical(distribution.metadata["Name"]) not in locked:
            continue
        for row in csv.reader((distribution.read_text("RECORD") or "").splitlines()):
            if len(row) == 3 and "/" not in row[0] and row[0].endswith(".pth") and "=" in row[1]:
                owned[row[0]] = tuple(row[1].split("=", 1))
    return owned


def file_digest(path, mode):
    digest = hashlib.new(mode, path.read_bytes())
    return base64.urlsafe_b64encode(digest.digest()).rstrip(b"=").decode()


def site_problems(site, owned, forbidden):
    # Return the shadowing and the startup-file problems of one site-packages directory.
    shadows, startup = [], []
    for entry in sorted(site.iterdir()) if site.is_dir() else ():
        name = entry.name
        if entry.suffix == ".pth":
            # The runner uses -S, so .pth files never run there; a locked wheel's unchanged file is accepted.
            if name not in owned or file_digest(entry, owned[name][0]) != owned[name][1]:
                startup.append(f"site-packages has the startup file {name}, which no locked wheel installs; remove it")
            continue
        module = module_name(entry)
        if module in STARTUP_MODULES:
            shadows.append(f"site-packages has {name}; remove the startup customization")
        elif module in forbidden:
            shadows.append(f"site-packages has {name}, which shadows the standard-library module {module}; remove it")
    return shadows, startup


def canonical(name):
    from packaging.utils import canonicalize_name

    return canonicalize_name(name)


def locked_versions(text):
    from packaging.version import Version

    return {
        canonical(match.group(1)): Version(match.group(2))
        for match in re.finditer(r"^([A-Za-z0-9][A-Za-z0-9._-]*)==([^\s;\\]+)", text, re.MULTILINE)
    }


def lock_problems(locked, distributions, supported_tags, sync):
    # uv pip sync --require-hashes checks integrity at installation; this finds later
    # version drift and wheels built for another interpreter or platform.
    from packaging.tags import parse_tag
    from packaging.version import Version

    problems = []
    installed = {canonical(distribution.metadata["Name"]): distribution for distribution in distributions}
    for name in sorted(locked.keys() - installed.keys()):
        problems.append(f"{name}=={locked[name]} from the lock is missing; {sync}")
    for name in sorted(installed.keys() - locked.keys() - UNLOCKED):
        problems.append(f"{name} is not in the lock; {sync}")
    for name in sorted(installed.keys() & locked.keys()):
        distribution = installed[name]
        if Version(distribution.version) != locked[name]:
            problems.append(f"{name} {distribution.version} differs from the locked {locked[name]}; {sync}")
        wheel = distribution.read_text("WHEEL") or ""
        tags = {tag for line in wheel.splitlines() if line.startswith("Tag:") for tag in parse_tag(line[4:].strip())}
        if not tags & supported_tags:
            problems.append(f"{name} is not a wheel for this interpreter and platform; {sync}")
    return problems


def dependency_problems(worker):
    from packaging.requirements import Requirement

    try:
        actual = deviations(metadata.distributions())
        if actual != EXPECTED:
            missing, unexpected = sorted(EXPECTED - actual), sorted(actual - EXPECTED)
            return [f"dependency deviations changed: missing={missing}, unexpected={unexpected}; {worker.SYNC}"]
        for name, floor in {"python-dotenv": ">=1.2.2", "fsspec": ">=2026.6.0", "pillow": ">=13"}.items():
            if not Requirement(name + floor).specifier.contains(metadata.version(name)):
                return [f"security floor not met: {name}; {worker.SYNC}"]
    except ValueError as error:
        return [f"{error}; {worker.SYNC}"]
    except metadata.PackageNotFoundError as error:
        return [f"{error.args[0] if error.args else 'a distribution'} is not installed; {worker.SYNC}"]
    try:
        # The worker's messages carry their own remedy.
        worker.verify_environment()
    except ValueError as error:
        return [str(error)]
    except metadata.PackageNotFoundError as error:
        name = error.args[0] if error.args else "a distribution"
        return [f"{name} is not installed; {worker.SOURCE if name == 'axolotl' else worker.SYNC}"]
    return []


def probe_environment(work, gpu_uuid):
    # The same variables as executionEnvironment in internal/llmtraining/run.go; the runner drops all others.
    environment = {
        "PATH": f"{Path(sys.executable).parent}:/usr/bin:/bin",
        "HOME": str(work / "home"), "XDG_CONFIG_HOME": str(work / "home"),
        "TMPDIR": str(work), "HF_HOME": str(work / "cache"),
        "HF_HUB_OFFLINE": "1", "HF_DATASETS_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1",
        "HF_HUB_DISABLE_TELEMETRY": "1", "AXOLOTL_DO_NOT_TRACK": "1", "DO_NOT_TRACK": "1",
        "WANDB_MODE": "disabled", "TOKENIZERS_PARALLELISM": "false", "LANG": "C.UTF-8",
    }
    if gpu_uuid is not None:
        environment["CUDA_VISIBLE_DEVICES"] = gpu_uuid
    return environment


def tail(path):
    with path.open("rb") as source:
        source.seek(max(0, path.stat().st_size - OUTPUT_LIMIT))
        return source.read(OUTPUT_LIMIT).decode("utf-8", "replace").splitlines()


def valid_facts(facts):
    def text(value, optional=True):
        return (optional and value is None) or (isinstance(value, str) and len(value) <= FACT_LENGTH)

    return (
        isinstance(facts, dict) and text(facts.get("torch"), False) and text(facts.get("cuda"))
        and text(facts.get("hip")) and text(facts.get("driverCuda"))
        and isinstance(facts.get("available"), bool) and isinstance(facts.get("bitsandbytes"), bool)
        and isinstance(facts.get("archList"), list) and len(facts["archList"]) <= FACT_ITEMS
        and all(text(arch, False) for arch in facts["archList"])
        and isinstance(facts.get("devices"), list) and len(facts["devices"]) <= FACT_ITEMS
        and all(isinstance(device, list) and len(device) == 2 and all(type(part) is int for part in device)
                for device in facts["devices"])
    )


def valid_probe(result):
    def message(value):
        return value is None or isinstance(value, str)

    return (
        isinstance(result, dict) and set(result) == {"stdlib", "facts", "torchError", "bitsandbytes"}
        and isinstance(result["stdlib"], list) and all(isinstance(item, str) for item in result["stdlib"])
        and (result["facts"] is None or valid_facts(result["facts"]))
        and message(result["torchError"]) and message(result["bitsandbytes"])
    )


def run_probe(quantized, gpu_uuid, popen=subprocess.Popen, killpg=os.killpg):
    # Return the runner-like child result, or None and an actionable failure.
    retry = "reinstall the training runtime from the lock and run the check again"
    with tempfile.TemporaryDirectory(prefix="training-check-") as directory:
        work = Path(directory)
        for name in ("home", "cache"):
            (work / name).mkdir(mode=0o700)
        result_path = work / "result.json"
        mode = "quantized" if quantized else "plain"
        command = [sys.executable, "-I", "-S", "-B", str(PROBE), str(WORKER), mode, str(result_path)]
        with open(work / "stdout", "wb") as stdout, open(work / "stderr", "wb") as stderr:
            process = popen(command, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr, cwd=work,
                            env=probe_environment(work, gpu_uuid), start_new_session=True)
            try:
                code = process.wait(timeout=PROBE_TIMEOUT)
            except subprocess.TimeoutExpired:
                killpg(process.pid, signal.SIGKILL)
                try:
                    process.wait(timeout=KILL_TIMEOUT)
                except subprocess.TimeoutExpired:
                    hung = "the runtime probe hung in the driver and did not stop"
                    return None, f"{hung}; check the NVIDIA driver and the GPU"
                return None, f"the runtime probe did not finish in {PROBE_TIMEOUT} seconds; {retry}"
        lines = tail(work / "stderr") or tail(work / "stdout")
        raw = result_path.read_bytes()[:OUTPUT_LIMIT + 1] if result_path.is_file() else b""
    if code < 0:
        name = signal.Signals(-code).name if -code in signal.valid_signals() else str(-code)
        remedy = "free memory or raise the memory limit" if code == -signal.SIGKILL else "check the system log"
        note = " (often out of memory)" if code == -signal.SIGKILL else ""
        return None, f"the runtime probe was killed by signal {name}{note}; {remedy} and run the check again"
    if code != 0:
        last = next((line.strip() for line in reversed(lines) if line.strip()), f"exit status {code}")
        return None, f"the runtime probe failed ({last}); {retry}"
    try:
        result = json.loads(raw) if len(raw) <= OUTPUT_LIMIT else None
    except ValueError:
        result = None
    if not valid_probe(result):
        return None, f"the runtime probe gave no valid result; {retry}"
    return result, None


def probe_checks(worker, probe, failure, target, gpu_uuid):
    # Map one probe result to the problems of the probe-based checks.
    if probe is None:
        dependent = "the runtime probe did not run; fix the native-libraries problem first"
        return {"stdlib": [failure], "native": [failure], "driver": [dependent], "accelerator": [dependent]}
    native = []
    if probe["torchError"]:
        native.append(f"Torch cannot load ({probe['torchError']}); reinstall torch and its CUDA wheels from the lock")
    if probe["bitsandbytes"]:
        native.append(probe["bitsandbytes"])
    facts = probe["facts"]
    accelerator = []
    if target != "cpu-smoke" and gpu_uuid is None:
        accelerator.append("no trainer GPU is selected; pass --gpu-uuid with the trainer GPU UUID")
    if facts is None:
        needs_torch = "this check needs Torch; fix the native-libraries problem first"
        accelerator.append(needs_torch)
        return {"stdlib": probe["stdlib"], "native": native, "driver": [needs_torch], "accelerator": accelerator}
    driver = []
    if facts["driverCuda"] is None:
        driver.append("the NVIDIA driver library libcuda.so.1 does not load; install the NVIDIA driver")
    accelerator.extend(worker.accelerator_problems(facts, target in QUANTIZED))
    return {"stdlib": probe["stdlib"], "native": native, "driver": driver, "accelerator": accelerator}


def check(name, problems, target, cpu_smoke_warning=False):
    # A cpu_smoke_warning check fails for the presets but only warns for the CPU smoke.
    if not problems:
        status = "pass"
    elif cpu_smoke_warning and target == "cpu-smoke":
        status = "warn"
    else:
        status = "fail"
    bounded = [mask(str(problem)) for problem in problems[:PROBLEM_LIMIT]]
    return {"name": name, "status": status, "problems": bounded, "omitted": max(0, len(problems) - PROBLEM_LIMIT)}


def guarded(name, compute, target, cpu_smoke_warning=False):
    try:
        problems = compute()
    except Exception as error:  # noqa: BLE001 - a broken check is a failure, never a traceback.
        problems = [f"check could not run ({type(error).__name__}: {error}); {CHECKER_BUG}"]
        cpu_smoke_warning = False
    return check(name, problems, target, cpu_smoke_warning)


def attempt(compute):
    try:
        return compute(), None
    except Exception as error:  # noqa: BLE001 - each dependent check reports the failure.
        return None, error


def picked(value, error, key):
    if error is not None:
        raise error
    return value[key]


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def report(worker, target, gpu_uuid=None, popen=subprocess.Popen):
    from packaging.tags import sys_tags

    distributions = list(metadata.distributions())
    locked = locked_versions(LOCK.read_text(encoding="utf-8"))
    # os came from the standard library before the site-packages insertion; sysconfig can
    # load a shadowing _sysconfigdata module from site-packages.
    stdlib = Path(os.__file__).parent
    site = worker.runtime_site()
    try:
        probe, failure = run_probe(target in QUANTIZED, gpu_uuid, popen)
    except Exception as error:  # noqa: BLE001 - the dependent checks report the failure.
        probe, failure = None, f"the runtime probe could not run ({type(error).__name__}); {CHECKER_BUG}"
    mapped, mapped_error = attempt(lambda: probe_checks(worker, probe, failure, target, gpu_uuid))
    forbidden = stdlib_names([stdlib, stdlib / "lib-dynload"])
    found, found_error = attempt(lambda: dict(zip(("shadows", "startup"), site_problems(
        site, owned_startup_files(distributions, locked), forbidden))))

    def python():
        problem = worker.python_problem((sys.version_info.major, sys.version_info.minor))
        return [problem] if problem else []

    checks = [
        guarded("python", python, target),
        guarded("runtime-layout", lambda: layout_problems(runtime_root(), stdlib, site), target, True),
        guarded("standard-library",
                lambda: picked(mapped, mapped_error, "stdlib") + picked(found, found_error, "shadows"), target),
        guarded("startup-files", lambda: picked(found, found_error, "startup"), target, cpu_smoke_warning=True),
        guarded("dependencies", lambda: dependency_problems(worker), target),
        guarded("locked-versions", lambda: lock_problems(locked, distributions, set(sys_tags()), worker.SYNC), target),
        guarded("native-libraries", lambda: picked(mapped, mapped_error, "native"), target),
        guarded("driver-libraries", lambda: picked(mapped, mapped_error, "driver"), target, cpu_smoke_warning=True),
        guarded("accelerator", lambda: picked(mapped, mapped_error, "accelerator"), target, cpu_smoke_warning=True),
    ]
    facts = probe["facts"] if probe is not None and probe["facts"] is not None else {}
    accelerator = None
    if facts:
        accelerator = "cuda" if facts["available"] else "cpu"
    result = skeleton(target)
    result.update(
        accelerator=accelerator, workerSHA256=digest(WORKER), lockSHA256=digest(LOCK), checks=checks,
        status="rejected" if any(item["status"] == "fail" for item in checks) else "accepted",
        **{key: facts.get(key) for key in STABLE_KEYS[1:]},
    )
    return result


def skeleton(target, error=None):
    # Every report has the same keys; unknown values are null or empty.
    result = {
        "version": 1, "target": target, "status": "rejected", "python": ".".join(map(str, sys.version_info[:3])),
        "lockTarget": LOCK_TARGET, "workerSHA256": None, "lockSHA256": None, "checks": [], "error": error,
    }
    result.update({key: None for key in STABLE_KEYS})
    return result


def main(args=None):
    # Exit 0 when the report is accepted, 1 when rejected and 2 on an internal error.
    # A usage error prints argparse usage text to stderr, no JSON, and exits 2.
    target = None
    try:
        presets = load_presets()
    except Exception as error:  # noqa: BLE001 - an internal error still gives one bounded JSON object.
        return internal_error(target, error)
    parser = argparse.ArgumentParser(description="Check the training runtime for one target. Run with -I -S -B.")
    parser.add_argument("--target", choices=("cpu-smoke", *presets), default="cpu-smoke")
    parser.add_argument("--gpu-uuid", help="trainer GPU UUID; the probe gets it as CUDA_VISIBLE_DEVICES")
    options = parser.parse_args(args)
    if options.gpu_uuid is not None and not GPU_UUID.fullmatch(options.gpu_uuid):
        parser.error("--gpu-uuid must be a GPU UUID such as GPU-01234567-89ab-cdef-0123-456789abcdef")
    if not (sys.flags.isolated and sys.flags.no_site):
        parser.error("run the check as the runner does: bin/python3 -I -S -B")
    target = options.target
    try:
        worker = load_module("training_worker", WORKER)
        site = str(worker.runtime_site())
        if site not in sys.path:
            sys.path.insert(0, site)
        result = report(worker, target, options.gpu_uuid)
    except Exception as error:  # noqa: BLE001 - an internal error still gives one bounded JSON object.
        return internal_error(target, error)
    print(json.dumps(result, indent=2, sort_keys=True))
    return 1 if result["status"] == "rejected" else 0


def internal_error(target, error):
    print(json.dumps(skeleton(target, mask(f"check could not run ({type(error).__name__}: {error}); {CHECKER_BUG}")),
                     indent=2, sort_keys=True))
    return 2


if __name__ == "__main__":
    sys.exit(main())
