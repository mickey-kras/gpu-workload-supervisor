"""Fail closed on every dependency deviation; do not rewrite upstream metadata.

Also check the training runtime for one target and print a bounded JSON report.
The report covers Python and its standard library, the installed distributions
against the lock, native library loading, and Torch and CUDA compatibility.
Run it as the runner does: bin/python3 -I -S -B. The `packaging` library is in the
runtime site-packages, which the checker adds only at run time. Thus the functions
import it locally, and report() loads every packaging module once before the probe runs.
"""
import sys

# Never write bytecode into the inventoried runtime root, whatever the flags.
sys.dont_write_bytecode = True

import argparse
import contextlib
from collections import deque
import hashlib
import importlib.abc
import importlib.metadata as metadata
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import tempfile
from typing import NamedTuple

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
FACT_KEYS = ("torch", "cuda", "driverCuda", "archList", "devices")
STABLE_KEYS = ("accelerator", *FACT_KEYS)


class SourceBytesLoader(importlib.abc.InspectLoader):
    # Load the worker from the hashed bytes. The probe gets a copy of the same bytes, so
    # the parent, the probe and workerSHA256 agree. A loader, not exec(), so that the import
    # system runs the code compiled from those bytes as a normal module; nothing reads the file again.
    def __init__(self, source, code):
        self.source, self.code = source, code

    def get_source(self, fullname):
        return self.source.decode("utf-8", "replace")

    def get_code(self, fullname):
        return self.code


class Sources(NamedTuple):
    # The repository bytes that the checker read once and that the probe gets copies of.
    worker: bytes
    probe: bytes


def read_probe():
    # Read the probe once. A probe that does not compile is a damaged repository input.
    source = read_input(PROBE)
    try:
        compile(source, str(PROBE), "exec", dont_inherit=True)
    except (SyntaxError, ValueError) as error:
        raise InputError(PROBE, error) from error
    return source


def load_worker():
    # Read the worker once. A worker that does not compile is a damaged repository input;
    # an exception while its module code runs stays a checker bug.
    source = read_input(WORKER)
    try:
        code = compile(source, str(WORKER), "exec", dont_inherit=True)
    except (SyntaxError, ValueError) as error:
        raise InputError(WORKER, error) from error
    spec = importlib.util.spec_from_loader("training_worker", SourceBytesLoader(source, code), origin=str(WORKER))
    worker = importlib.util.module_from_spec(spec)
    worker.__file__ = str(WORKER)
    spec.loader.exec_module(worker)
    return worker, source


class InputError(Exception):
    # A repository input file is missing or broken; the remedy names the file to restore.
    def __init__(self, path, error):
        super().__init__(f"{path.name}: {type(error).__name__}: {error}")
        repository = HERE.parents[1]
        shown = path.relative_to(repository) if path.is_relative_to(repository) else path.name
        self.remedy = f"restore {shown} from the repository checkout"


def read_input(path):
    try:
        return path.read_bytes()
    except OSError as error:
        raise InputError(path, error) from error


def load_presets(path=None):
    path = path or ASSETS
    try:
        presets = json.loads(read_input(path))
        if not isinstance(presets, dict) or not presets or not all(isinstance(name, str) for name in presets):
            raise ValueError("the asset table has no presets")
    except ValueError as error:
        raise InputError(path, error) from error
    return tuple(presets)


def load_packaging():
    # Import every packaging module that the checker uses before the probe runs. Later
    # local imports then reuse these modules, so files that the probe changed do not run here.
    for name in ("packaging.requirements", "packaging.tags", "packaging.utils", "packaging.version"):
        importlib.import_module(name)


def current_directory():
    # A deleted working directory has no name to hide.
    try:
        return os.getcwd()
    except OSError:
        return None


def mask(text):
    # Report no host paths: hide the known roots literally, then any other absolute path.
    literals = {
        os.environ.get("HOME", ""): "<home>", tempfile.gettempdir(): "<tmp>",
        sys.base_prefix: "<python>", sys.prefix: "<python>", str(runtime_root()): "<runtime>",
    }
    cwd = current_directory()
    if cwd is not None:
        literals.setdefault(cwd, "<cwd>")
    for literal in sorted(literals, key=len, reverse=True):
        if len(literal) > 1:
            # Replace a root only at a path boundary, so /home does not change /homework.
            text = re.sub(re.escape(literal) + r"(?=/|\s|$|[;:,()'\"])", literals[literal], text)
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
    if suffix not in {".py", ".pyc"}:
        return None
    return stem or None


def stdlib_names(directories):
    # sys.stdlib_module_names omits private modules such as _sysconfigdata_*, so add the
    # private names from the directories. Public extras, such as the test package that
    # some builds ship, are not part of the runtime contract.
    names = set(sys.stdlib_module_names)
    for directory in directories:
        if directory.is_dir():
            names.update(name for name in map(module_name, directory.iterdir()) if name and name.startswith("_"))
    return names


def owned_startup_files(distributions, locked):
    # Return the top-level .pth files that the RECORD of a locked wheel lists.
    return {
        str(file) for distribution in distributions if canonical(distribution.metadata["Name"]) in locked
        for file in distribution.files or () if "/" not in str(file) and str(file).endswith(".pth")
    }


def site_problems(site, owned, forbidden):
    # Return the shadowing and the startup-file problems of one site-packages directory.
    shadows, startup = [], []
    for entry in sorted(site.iterdir()) if site.is_dir() else ():
        name = entry.name
        if entry.suffix == ".pth":
            # The runner and the checker use -S, but subprocesses of the training stack can process
            # .pth files. Accept only a file that a locked wheel's RECORD lists; uv pip sync
            # --require-hashes, not this check, gives the integrity of its bytes.
            if name not in owned:
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
    # The same variables as executionEnvironment in internal/llmtraining/run.go, except
    # CUDA_VISIBLE_DEVICES when no GPU is selected. The runner drops all other variables.
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


def tail(source):
    # Read through the file object that the parent created, never by path: the probe can
    # replace the path with a FIFO.
    size = source.seek(0, os.SEEK_END)
    source.seek(max(0, size - OUTPUT_LIMIT))
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


def stop_group(process, killpg):
    # The probe runs in its own session; stop every process that it left behind.
    with contextlib.suppress(ProcessLookupError):
        killpg(process.pid, signal.SIGKILL)


def wait_for_probe(process, killpg):
    # Return the exit code and None, or None and an actionable failure when the probe does not finish.
    driver = "check the NVIDIA driver and the GPU, then run the check again"
    try:
        code = process.wait(timeout=PROBE_TIMEOUT)
    except subprocess.TimeoutExpired:
        code = None
    stop_group(process, killpg)
    if code is not None:
        return code, None
    try:
        process.wait(timeout=KILL_TIMEOUT)
    except subprocess.TimeoutExpired:
        return None, f"the runtime probe hung in the driver and did not stop; {driver}"
    return None, f"the runtime probe did not finish in {PROBE_TIMEOUT} seconds; {driver}"


def read_result(path):
    # Read at most one byte over the limit from a regular file. Never follow a symbolic
    # link, and never block on a FIFO or a device.
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError:
        return b""
    try:
        with os.fdopen(descriptor, "rb") as source:
            if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
                return b""
            return source.read(OUTPUT_LIMIT + 1)
    except OSError:
        return b""


def decode_probe(raw):
    # Return a valid probe result, or None.
    if len(raw) > OUTPUT_LIMIT:
        return None
    try:
        result = json.loads(raw)
    except ValueError:
        return None
    return result if valid_probe(result) else None


def exit_failure(code, lines, retry):
    if code < 0:
        name = signal.Signals(-code).name if -code in signal.valid_signals() else str(-code)
        if code == -signal.SIGKILL:
            return (f"the runtime probe was killed by signal {name} (often out of memory); "
                    "free memory or raise the memory limit and run the check again")
        return f"the runtime probe was killed by signal {name}; check the system log and run the check again"
    last = next((line.strip() for line in reversed(lines) if line.strip()), f"exit status {code}")
    return f"the runtime probe failed ({last}); {retry}"


def write_private(path, data):
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as target:
        target.write(data)


def run_probe(quantized, gpu_uuid, sources, popen=subprocess.Popen, killpg=os.killpg):
    # Return the runner-like child result, or None and an actionable failure.
    retry = "reinstall the training runtime from the lock and run the check again"
    with tempfile.TemporaryDirectory(prefix="training-check-") as directory:
        work = Path(directory)
        for name in ("home", "cache"):
            (work / name).mkdir(mode=0o700)
        # Like run.go, give the child private copies of the bytes that the checker read once,
        # so a later change to either repository file cannot reach the child.
        probe_path, worker_path, result_path = work / "runtime_probe.py", work / "worker.py", work / "result.json"
        write_private(worker_path, sources.worker)
        write_private(probe_path, sources.probe)
        mode = "quantized" if quantized else "plain"
        command = [sys.executable, "-I", "-S", "-B", str(probe_path), str(worker_path), mode, str(result_path)]
        with open(work / "stdout", "w+b") as stdout, open(work / "stderr", "w+b") as stderr:
            process = popen(command, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr, cwd=work,
                            env=probe_environment(work, gpu_uuid), start_new_session=True)
            code, failure = wait_for_probe(process, killpg)
            if failure is not None:
                return None, failure
            lines = tail(stderr) or tail(stdout)
        raw = read_result(result_path)
    if code != 0:
        return None, exit_failure(code, lines, retry)
    result = decode_probe(raw)
    if result is None:
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


def remedy_for(error, sync=None):
    # A broken repository input names the file to restore. A missing module or an unreadable
    # runtime file is an installation problem when the sync remedy is known. Any other
    # error is a checker bug.
    if isinstance(error, InputError):
        return error.remedy
    if sync is not None and isinstance(error, (ImportError, OSError)):
        return sync
    return CHECKER_BUG


def guarded(name, compute, target, sync, cpu_smoke_warning=False):
    try:
        problems = compute()
    except Exception as error:  # noqa: BLE001
        # A broken check is a failure, never a traceback.
        problems = [f"check could not run ({type(error).__name__}: {error}); {remedy_for(error, sync)}"]
        cpu_smoke_warning = False
    return check(name, problems, target, cpu_smoke_warning)


def attempt(compute):
    try:
        return compute(), None
    except Exception as error:  # noqa: BLE001
        # Each dependent check reports the failure.
        return None, error


def picked(value, error, key):
    # Return one key of the shared value, or raise the error that computing it gave.
    if error is not None:
        raise error
    return value[key]


def digest(data):
    return hashlib.sha256(data).hexdigest()


def parse_lock(lock):
    # A lock that does not decode or has a bad version is a damaged repository input.
    try:
        return locked_versions(lock.decode("utf-8"))
    except ValueError as error:
        raise InputError(LOCK, error) from error


def probe_with_rescan(worker, sources, target, gpu_uuid, popen, scan):
    # Scan site-packages, run the probe, then scan again: the probe imports site-packages
    # first, as the runner does, so planted code can run there and hide itself.
    before, before_error = attempt(scan)
    try:
        probe, failure = run_probe(target in QUANTIZED, gpu_uuid, sources, popen)
    except Exception as error:  # noqa: BLE001
        # The dependent checks report the failure.
        remedy = remedy_for(error, worker.SYNC)
        probe, failure = None, f"the runtime probe could not start ({type(error).__name__}: {error}); {remedy}"
    found, found_error = attempt(scan)
    if before_error is not None or found_error is not None:
        # A failed scan must not pass on the other scan alone.
        return probe, failure, None, before_error or found_error
    if before != found:
        # This goes to standard-library on purpose: a change means the probe ran planted code.
        found["shadows"].append(f"site-packages changed while the runtime probe ran; {worker.SYNC}")
    return probe, failure, found, None


class Inputs(NamedTuple):
    # The shared values of one report. mapped and found are (value, error) pairs; each
    # check reads them through picked().
    locked: dict
    distributions: list
    mapped: tuple
    found: tuple
    stdlib: Path
    site: Path


def build_checks(worker, target, inputs):
    locked, distributions, mapped, found, stdlib, site = inputs

    def python():
        problem = worker.python_problem((sys.version_info.major, sys.version_info.minor))
        return [problem] if problem else []

    def versions():
        from packaging.tags import sys_tags

        return lock_problems(locked, distributions, set(sys_tags()), worker.SYNC)

    def guard(name, compute, cpu_smoke_warning=False):
        return guarded(name, compute, target, worker.SYNC, cpu_smoke_warning=cpu_smoke_warning)

    return [
        guard("python", python),
        guard("runtime-layout", lambda: layout_problems(runtime_root(), stdlib, site), cpu_smoke_warning=True),
        guard("standard-library", lambda: picked(*mapped, "stdlib") + picked(*found, "shadows")),
        guard("startup-files", lambda: picked(*found, "startup"), cpu_smoke_warning=True),
        guard("dependencies", lambda: dependency_problems(worker)),
        guard("locked-versions", versions),
        guard("native-libraries", lambda: picked(*mapped, "native")),
        guard("driver-libraries", lambda: picked(*mapped, "driver"), cpu_smoke_warning=True),
        guard("accelerator", lambda: picked(*mapped, "accelerator"), cpu_smoke_warning=True),
    ]


def report(worker, target, *, worker_source, gpu_uuid=None, popen=subprocess.Popen):
    load_packaging()
    lock = read_input(LOCK)
    sources = Sources(worker_source, read_probe())
    locked = parse_lock(lock)
    distributions = list(metadata.distributions())
    # os came from the standard library before site-packages was added; sysconfig can
    # load a shadowing _sysconfigdata module from site-packages.
    stdlib = Path(os.__file__).parent
    site = worker.runtime_site()
    forbidden = stdlib_names([stdlib, stdlib / "lib-dynload"])

    def scan():
        owned = owned_startup_files(distributions, locked)
        return dict(zip(("shadows", "startup"), site_problems(site, owned, forbidden)))

    probe, failure, found, found_error = probe_with_rescan(worker, sources, target, gpu_uuid, popen, scan)
    mapped = attempt(lambda: probe_checks(worker, probe, failure, target, gpu_uuid))
    checks = build_checks(worker, target, Inputs(locked, distributions, mapped, (found, found_error), stdlib, site))
    facts = probe["facts"] if probe is not None and probe["facts"] is not None else {}
    accelerator = None
    if facts:
        accelerator = "cuda" if facts["available"] else "cpu"
    result = skeleton(target)
    result.update(
        accelerator=accelerator, workerSHA256=digest(worker_source), lockSHA256=digest(lock), checks=checks,
        status="rejected" if any(item["status"] == "fail" for item in checks) else "accepted",
        **{key: facts.get(key) for key in FACT_KEYS},
    )
    return result


def skeleton(target, error=None):
    # Every report has the same keys; unknown values are null or empty.
    result = {
        "version": 1, "target": target, "status": "rejected", "python": ".".join(map(str, sys.version_info[:3])),
        "lockTarget": LOCK_TARGET, "workerSHA256": None, "lockSHA256": None, "checks": [], "error": error,
    }
    result.update(dict.fromkeys(STABLE_KEYS))
    return result


def main(args=None):
    # Exit 0 when the report is accepted, 1 when rejected and 2 on an internal error.
    # A usage error prints argparse usage text to stderr, no JSON, and exits 2.
    target = None
    try:
        presets = load_presets()
    except Exception as error:  # noqa: BLE001
        # An internal error still gives one bounded JSON object.
        return internal_error(target, error, remedy_for(error))
    parser = argparse.ArgumentParser(description="Check the training runtime for one target. Run with -I -S -B.")
    parser.add_argument("--target", choices=("cpu-smoke", *presets), default="cpu-smoke")
    parser.add_argument("--gpu-uuid", help="trainer GPU UUID; the probe gets it as CUDA_VISIBLE_DEVICES")
    options = parser.parse_args(args)
    if options.gpu_uuid is not None and not GPU_UUID.fullmatch(options.gpu_uuid):
        parser.error("--gpu-uuid must be a GPU UUID such as GPU-01234567-89ab-cdef-0123-456789abcdef")
    if not (sys.flags.isolated and sys.flags.no_site):
        parser.error("run the check as the runner does: bin/python3 -I -S -B")
    target, worker = options.target, None
    try:
        worker, worker_source = load_worker()
        # Append, unlike the worker: a planted standard-library name in site-packages must
        # not run in the checker before the shadowing scan finds it. The probe uses the runner order.
        site = str(worker.runtime_site())
        if site not in sys.path:
            sys.path.append(site)
        result = report(worker, target, worker_source=worker_source, gpu_uuid=options.gpu_uuid)
    except Exception as error:  # noqa: BLE001
        # An internal error still gives one bounded JSON object.
        return internal_error(target, error, remedy_for(error, worker.SYNC if worker is not None else None))
    print(json.dumps(result, indent=2, sort_keys=True))
    return 1 if result["status"] == "rejected" else 0


def internal_error(target, error, remedy):
    print(json.dumps(skeleton(target, mask(f"check could not run ({type(error).__name__}: {error}); {remedy}")),
                     indent=2, sort_keys=True))
    return 2


if __name__ == "__main__":
    sys.exit(main())

