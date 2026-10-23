"""Fail closed on every dependency deviation; do not rewrite upstream metadata.

Also check the training runtime for one target and print a bounded JSON report.
The report covers Python and its standard library, the installed distributions
against the lock, native library loading, and Torch and CUDA compatibility.
"""
import sys

# Never write bytecode into the inventoried runtime root, whatever the flags.
sys.dont_write_bytecode = True

import argparse
from collections import deque
import hashlib
import importlib.metadata as metadata
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sysconfig
import tempfile

from packaging.requirements import Requirement
from packaging.utils import canonicalize_name
from packaging.version import Version

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
    installed = {}
    for distribution in distributions:
        name = canonicalize_name(distribution.metadata["Name"])
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
        name = canonicalize_name(requirement.name)
        dependency = installed.get(name)
        if dependency is None:
            result.add((parent, name, str(requirement.specifier), "missing"))
        elif requirement.url or not requirement.specifier.contains(dependency.version, prereleases=True):
            result.add((parent, name, str(requirement.specifier), "version"))
        if dependency is not None:
            requested_extras.extend((name, canonicalize_name(requested)) for requested in requirement.extras)
    return result, requested_extras


HERE = Path(__file__).resolve().parent
LOCK = HERE / "requirements.txt"
WORKER = HERE.parents[1] / "internal/llmtraining/worker.py"
PROBE = HERE / "runtime_probe.py"
# requirements.in names the interpreter that the lock resolves for.
LOCK_TARGET = "CPython 3.12.14"
# The presets come from the reference asset table; QLoRA is the quantized one.
PRESETS = tuple(json.loads((HERE.parent / "assets.json").read_text(encoding="utf-8")))
QUANTIZED = {"representative-qlora"}
TARGETS = ("cpu-smoke", *PRESETS)
# Only the Axolotl source is installed outside the hash-locked wheel set.
UNLOCKED = {"axolotl"}
STARTUP_MODULES = {"sitecustomize", "usercustomize"}
PROBE_TIMEOUT = 300
OUTPUT_LIMIT = 64 << 10
PROBLEM_LIMIT = 20
PROBLEM_LENGTH = 300
GPU_UUID = re.compile(r"(GPU|MIG)-[0-9A-Fa-f-]{1,64}")


def mask(text):
    # Report no host paths: hide the runtime root first, then any other absolute path.
    text = text.replace(sys.prefix, "<runtime>")
    text = re.sub(r"(?<![\w.<>-])/[^\s;:,()'\"]+", "<path>", text)
    return text[:PROBLEM_LENGTH]


def layout_problems(prefix, base_prefix, stdlib):
    # The runner executes bin/python3 from one inventoried root without symlinks.
    problems = []
    if prefix != base_prefix:
        problems.append("the runtime is a virtual environment; the runner needs a self-contained CPython root")
    executable = prefix / "bin" / "python3"
    if executable.is_symlink() or not executable.is_file():
        problems.append("bin/python3 is not a regular file; copy the interpreter into the runtime root")
    site = prefix / "lib" / f"python{sys.version_info.major}.{sys.version_info.minor}" / "site-packages"
    if site.is_symlink() or not site.is_dir():
        problems.append(f"{site.relative_to(prefix)} is not a regular directory in the runtime root")
    if not stdlib.is_relative_to(prefix):
        problems.append("the standard library is outside the runtime root; install it into the same root")
    return problems


def site_problems(site, owned):
    # Return the shadowing and the startup-file problems of one site-packages directory.
    shadows, startup = [], []
    for entry in sorted(site.iterdir()) if site.is_dir() else ():
        name, top = entry.name, entry.name.split(".")[0]
        if entry.suffix == ".pth":
            # The runner uses -S, so .pth files never run there; a locked wheel's own file is accepted.
            if name not in owned:
                startup.append(f"site-packages has the startup file {name}, which no locked wheel installs; remove it")
        elif top in STARTUP_MODULES:
            shadows.append(f"site-packages has {name}; remove the startup customization")
        elif top in sys.stdlib_module_names and (entry.is_dir() or entry.suffix in {".py", ".pyc", ".so"}):
            shadows.append(f"site-packages has {name}, which shadows the standard-library module {top}; remove it")
    return shadows, startup


def owned_startup_files(distributions, locked):
    return {
        str(file) for distribution in distributions
        if canonicalize_name(distribution.metadata["Name"]) in locked
        for file in distribution.files or () if "/" not in str(file) and str(file).endswith(".pth")
    }


def locked_versions(text):
    return {
        canonicalize_name(match.group(1)): Version(match.group(2))
        for match in re.finditer(r"^([A-Za-z0-9][A-Za-z0-9._-]*)==([^\s;\\]+)", text, re.MULTILINE)
    }


def lock_problems(locked, distributions):
    # uv pip sync --require-hashes checks integrity at installation; this finds later version drift.
    problems = []
    installed = {canonicalize_name(distribution.metadata["Name"]): distribution for distribution in distributions}
    for name in sorted(locked.keys() - installed.keys()):
        problems.append(
            f"{name}=={locked[name]} from the lock is missing; "
            "run uv pip sync --require-hashes with the lock"
        )
    for name in sorted(installed.keys() - locked.keys() - UNLOCKED):
        problems.append(f"{name} is not in the lock; run uv pip sync --require-hashes with the lock to remove it")
    for name in sorted(installed.keys() & locked.keys()):
        if Version(installed[name].version) != locked[name]:
            problems.append(
                f"{name} {installed[name].version} differs from the locked {locked[name]}; "
                "run uv pip sync --require-hashes with the lock"
            )
    return problems


def dependency_problems(worker):
    sync = "reinstall the training runtime from the lock with uv pip sync --require-hashes"
    source = f"install axolotl noneditable with --no-deps from commit {worker.SOURCE_REVISION}"
    try:
        actual = deviations(metadata.distributions())
        if actual != EXPECTED:
            missing, unexpected = sorted(EXPECTED - actual), sorted(actual - EXPECTED)
            return [f"dependency deviations changed: missing={missing}, unexpected={unexpected}; {sync}"]
        for name, floor in {"python-dotenv": ">=1.2.2", "fsspec": ">=2026.6.0", "pillow": ">=13"}.items():
            if not Requirement(name + floor).specifier.contains(metadata.version(name)):
                return [f"security floor not met: {name}; {sync}"]
        worker.verify_environment()
    except metadata.PackageNotFoundError as error:
        name = error.args[0] if error.args else "a distribution"
        return [f"{name} is not installed; {source if name == 'axolotl' else sync}"]
    except ValueError as error:
        remedy = {
            "runtime source is not pinned": source,
            "unapproved runtime entry point": "remove the distribution that registers it",
        }.get(str(error), "run uv pip sync --require-hashes with the lock to remove it")
        return [f"{error}; {remedy}"]
    return []


def probe_environment(work, gpu_uuid):
    # The same variables as executionEnvironment in internal/llmtraining/run.go.
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


def run_probe(quantized, gpu_uuid, run=subprocess.run):
    # Return the runner-like child result, or None and an actionable failure.
    retry = "reinstall the training runtime from the lock and run the check again"
    with tempfile.TemporaryDirectory(prefix="training-check-") as directory:
        work = Path(directory)
        for name in ("home", "cache"):
            (work / name).mkdir(mode=0o700)
        command = [sys.executable, "-I", "-S", "-B", str(PROBE), str(WORKER), "quantized" if quantized else "plain"]
        with open(work / "stdout", "wb") as stdout, open(work / "stderr", "wb") as stderr:
            try:
                completed = run(command, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr, cwd=work,
                                env=probe_environment(work, gpu_uuid), timeout=PROBE_TIMEOUT, check=False)
            except subprocess.TimeoutExpired:
                return None, f"the runtime probe did not finish in {PROBE_TIMEOUT} seconds; {retry}"
        output = (work / "stdout").read_bytes()[-OUTPUT_LIMIT:].decode("utf-8", "replace").splitlines()
        errors = (work / "stderr").read_bytes()[-OUTPUT_LIMIT:].decode("utf-8", "replace").splitlines()
    if completed.returncode != 0:
        last = next((line.strip() for line in reversed(errors) if line.strip()), f"exit status {completed.returncode}")
        return None, f"the runtime probe failed ({last}); {retry}"
    try:
        result = json.loads(output[-1])
    except (IndexError, ValueError):
        result = None
    if not isinstance(result, dict):
        return None, f"the runtime probe gave no valid result; {retry}"
    return result, None


def probe_checks(probe, failure, target, gpu_uuid, environment):
    # Map one probe result to the problems of the probe-based checks.
    if probe is None:
        dependent = "the runtime probe did not run; fix the native-libraries problem first"
        return {"stdlib": [failure], "native": [failure], "driver": [dependent], "accelerator": [dependent]}
    native = []
    if probe["torchError"]:
        native.append(f"Torch cannot load ({probe['torchError']}); reinstall torch and its CUDA wheels from the lock")
    if probe["bitsandbytes"]:
        native.append(probe["bitsandbytes"])
    quantized = target in QUANTIZED
    if quantized and "BNB_CUDA_VERSION" in environment:
        native.append("BNB_CUDA_VERSION is set, which the training runtime does not support; unset it")
    driver = []
    if not probe["driverCuda"]:
        driver.append("the NVIDIA driver library libcuda.so.1 does not load; install the NVIDIA driver")
    facts = probe["facts"]
    accelerator = []
    if target != "cpu-smoke" and gpu_uuid is None:
        accelerator.append("no trainer GPU is selected; pass --gpu-uuid with the trainer GPU UUID")
    if facts is None:
        accelerator.append("the accelerator check needs Torch; fix the native-libraries problem first")
    else:
        accelerator.extend(WORKER_MODULE.accelerator_problems(facts, quantized))
    return {"stdlib": probe["stdlib"], "native": native, "driver": driver, "accelerator": accelerator}


def check(name, problems, target, cpu_smoke_warning=False):
    # A cpu_smoke_warning check fails for the presets but only warns for the CPU smoke.
    status = "pass" if not problems else "warn" if cpu_smoke_warning and target == "cpu-smoke" else "fail"
    bounded = [mask(str(problem)) for problem in problems[:PROBLEM_LIMIT]]
    return {"name": name, "status": status, "problems": bounded, "omitted": max(0, len(problems) - PROBLEM_LIMIT)}


def guarded(name, compute, target, cpu_smoke_warning=False):
    try:
        problems = compute()
    except Exception as error:  # noqa: BLE001 - a broken check is a failure, never a traceback.
        problems = [f"check could not run: {type(error).__name__}"]
        return {"name": name, "status": "fail", "problems": problems, "omitted": 0}
    return check(name, problems, target, cpu_smoke_warning)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def report(target, gpu_uuid=None, run=subprocess.run):
    distributions = list(metadata.distributions())
    locked = locked_versions(LOCK.read_text(encoding="utf-8"))
    paths = sysconfig.get_paths()
    try:
        probe, failure = run_probe(target in QUANTIZED, gpu_uuid, run)
    except Exception as error:  # noqa: BLE001 - the dependent checks report the failure.
        probe, failure = None, f"the runtime probe could not run: {type(error).__name__}"

    def mapped(key):
        return probe_checks(probe, failure, target, gpu_uuid, os.environ)[key]

    def site(index):
        return site_problems(WORKER_MODULE.runtime_site(), owned_startup_files(distributions, locked))[index]

    def python():
        problem = WORKER_MODULE.python_problem((sys.version_info.major, sys.version_info.minor))
        return [problem] if problem else []

    def layout():
        return layout_problems(Path(sys.prefix), Path(sys.base_prefix), Path(paths["stdlib"]))

    checks = [
        guarded("python", python, target),
        guarded("runtime-layout", layout, target, cpu_smoke_warning=True),
        guarded("standard-library", lambda: mapped("stdlib") + site(0), target),
        guarded("startup-files", lambda: site(1), target, cpu_smoke_warning=True),
        guarded("dependencies", lambda: dependency_problems(WORKER_MODULE), target),
        guarded("locked-versions", lambda: lock_problems(locked, distributions), target),
        guarded("native-libraries", lambda: mapped("native"), target),
        guarded("driver-libraries", lambda: mapped("driver"), target, cpu_smoke_warning=True),
        guarded("accelerator", lambda: mapped("accelerator"), target, cpu_smoke_warning=True),
    ]
    facts = probe.get("facts") if isinstance(probe, dict) else None
    facts = facts if isinstance(facts, dict) else {}
    return {
        "version": 1, "target": target, "python": ".".join(map(str, sys.version_info[:3])),
        "lockTarget": LOCK_TARGET, "workerSHA256": digest(WORKER), "lockSHA256": digest(LOCK),
        "accelerator": ("cuda" if facts["available"] else "cpu") if "available" in facts else None,
        **{key: facts.get(key) for key in ("torch", "cuda", "driverCuda", "archList", "devices")},
        "checks": checks, "status": "rejected" if any(item["status"] == "fail" for item in checks) else "accepted",
    }


def load_worker():
    spec = importlib.util.spec_from_file_location("training_worker", WORKER)
    worker = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(worker)
    return worker


WORKER_MODULE = load_worker()


def main(args=None):
    # Exit 0 when the report is accepted, 1 when rejected and 2 on a usage or internal error.
    parser = argparse.ArgumentParser(description="Check the training runtime for one target.")
    parser.add_argument("--target", choices=TARGETS, default="cpu-smoke")
    parser.add_argument("--gpu-uuid", help="trainer GPU UUID; the probe gets it as CUDA_VISIBLE_DEVICES")
    options = parser.parse_args(args)
    if options.gpu_uuid is not None and not GPU_UUID.fullmatch(options.gpu_uuid):
        parser.error("--gpu-uuid must be a GPU or MIG UUID")
    try:
        result = report(options.target, options.gpu_uuid)
    except Exception as error:  # noqa: BLE001 - an internal error still gives one bounded JSON object.
        print(json.dumps({"version": 1, "target": options.target, "status": "rejected",
                          "error": f"check could not run: {type(error).__name__}"}))
        return 2
    print(json.dumps(result, indent=2, sort_keys=True))
    return 1 if result["status"] == "rejected" else 0


if __name__ == "__main__":
    sys.exit(main())
