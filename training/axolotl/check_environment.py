"""Fail closed on every dependency deviation; do not rewrite upstream metadata.

Also check the complete training runtime for one target and print a bounded JSON
report: Python and its standard library, installed wheels against the lock,
native library resolution and Torch/CUDA compatibility with the visible GPUs.
"""
import argparse
import base64
from collections import deque
import csv
import hashlib
import importlib
import importlib.metadata as metadata
import importlib.util
import json
from pathlib import Path
import re
import struct
import sys
import sysconfig

from packaging.requirements import Requirement
from packaging.tags import parse_tag, sys_tags
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


LOCK = Path(__file__).resolve().with_name("requirements.txt")
PYTHON = (3, 12)
TARGETS = ("cpu-smoke", "smoke-lora", "representative-qlora")
# Only the Axolotl source is installed outside the hash-locked wheel set.
UNLOCKED = {"axolotl"}
# Standard-library modules with native parts that the training stack imports.
STDLIB_MODULES = ("_bz2", "_ctypes", "_decimal", "_hashlib", "_lzma", "_sqlite3", "_ssl", "zlib")
# Glibc default directories; the loader cache adds the configured ones. The
# runner sets no LD_LIBRARY_PATH, so the check ignores it as well.
SYSTEM_DIRECTORIES = ("/lib64", "/usr/lib64", "/lib", "/usr/lib")
# The NVIDIA driver, not a wheel, gives these libraries on a GPU host.
DRIVER_LIBRARIES = {"libcuda.so.1", "libnvidia-ml.so.1"}
PROBLEM_LIMIT = 20


def python_problems(implementation, version):
    if implementation != "cpython" or tuple(version[:2]) != PYTHON:
        return [f"the runtime is {implementation} {'.'.join(map(str, version[:3]))}; the training lock targets CPython 3.12, so install CPython 3.12"]
    return []


def layout_problems(prefix, base_prefix, stdlib):
    # The runner executes bin/python3 from one inventoried root without symlinks.
    problems = []
    if prefix != base_prefix:
        problems.append("the runtime is a virtual environment; the runner needs a self-contained CPython root")
    executable = prefix / "bin" / "python3"
    if executable.is_symlink() or not executable.is_file():
        problems.append("bin/python3 is not a regular file; copy the interpreter into the runtime root")
    site = prefix / "lib" / f"python{PYTHON[0]}.{PYTHON[1]}" / "site-packages"
    if site.is_symlink() or not site.is_dir():
        problems.append("lib/python3.12/site-packages is not a regular directory in the runtime root")
    if not stdlib.is_relative_to(prefix):
        problems.append("the standard library is outside the runtime root; install it into the same root")
    return problems


def stdlib_problems(directories, modules=STDLIB_MODULES):
    problems = []
    for name in modules:
        try:
            spec = importlib.util.find_spec(name)
            importlib.import_module(name)
        except (ImportError, OSError):
            problems.append(f"standard-library module {name} cannot load; rebuild or reinstall CPython with its system libraries")
            continue
        if spec is None or (spec.origin not in {"built-in", "frozen"} and not any(Path(spec.origin).is_relative_to(path) for path in directories)):
            problems.append(f"standard-library module {name} is outside the standard library; remove the shadowing copy")
    return problems


def locked_versions(text):
    return {
        canonicalize_name(match.group(1)): Version(match.group(2))
        for match in re.finditer(r"^([A-Za-z0-9][A-Za-z0-9._-]*)==([^\s;\\]+)", text, re.MULTILINE)
    }


def lock_problems(locked, distributions, supported_tags):
    problems = []
    installed = {canonicalize_name(distribution.metadata["Name"]): distribution for distribution in distributions}
    for name in sorted(locked.keys() - installed.keys()):
        problems.append(f"{name}=={locked[name]} from the lock is missing; run uv pip sync --require-hashes with the lock")
    for name in sorted(installed.keys() - locked.keys() - UNLOCKED):
        problems.append(f"{name} is not in the lock; remove it from the training runtime")
    for name in sorted(installed.keys() & locked.keys()):
        distribution = installed[name]
        if Version(distribution.version) != locked[name]:
            problems.append(f"{name} {distribution.version} differs from the locked {locked[name]}; run uv pip sync with the lock")
        wheel = distribution.read_text("WHEEL") or ""
        tags = {tag for line in wheel.splitlines() if line.startswith("Tag: ") for tag in parse_tag(line[5:].strip())}
        if not tags & supported_tags:
            problems.append(f"{name} is not an installed wheel for this interpreter and platform; reinstall it from the lock")
    return problems


def record_problems(distributions):
    # Installed bytes must still match each wheel RECORD; nothing is rewritten.
    # Read RECORD directly: Distribution.files omits missing files.
    problems = []
    for distribution in sorted(distributions, key=lambda value: canonicalize_name(value.metadata["Name"])):
        name = canonicalize_name(distribution.metadata["Name"])
        for row in csv.reader((distribution.read_text("RECORD") or "").splitlines()):
            if len(row) != 3 or "=" not in row[1]:
                continue
            mode, value = row[1].split("=", 1)
            path = Path(distribution.locate_file(row[0]))
            if path.is_symlink() or not path.is_file() or (row[2] and path.stat().st_size != int(row[2])) or file_digest(path, mode) != value:
                problems.append(f"{name} file {row[0]} differs from its RECORD; reinstall {name} from the lock")
    return problems


def file_digest(path, mode):
    digest = hashlib.new(mode)
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1 << 20), b""):
            digest.update(block)
    return base64.urlsafe_b64encode(digest.digest()).rstrip(b"=").decode()


def elf_dynamic(path):
    """Return the machine, DT_NEEDED names and search paths of a 64-bit little-endian ELF."""
    with path.open("rb") as source:
        header = source.read(64)
        if len(header) < 64 or header[:6] != b"\x7fELF\x02\x01":
            return None
        machine, program = struct.unpack_from("<H", header, 18)[0], struct.unpack_from("<Q", header, 32)[0]
        size, count = struct.unpack_from("<HH", header, 54)
        source.seek(program)
        table = source.read(size * count)
        loads, dynamic = [], None
        for offset in range(0, len(table) - 39, max(size, 40)):
            kind, _, file_offset, address, _, length = struct.unpack_from("<IIQQQQ", table, offset)
            if kind == 1:
                loads.append((address, file_offset, length))
            elif kind == 2:
                dynamic = (file_offset, length)
        if dynamic is None:
            return machine, [], []
        source.seek(dynamic[0])
        raw = source.read(min(dynamic[1], 1 << 20))
        entries = []
        for offset in range(0, len(raw) - 15, 16):
            entry = struct.unpack_from("<qQ", raw, offset)
            if entry[0] == 0:
                break
            entries.append(entry)
        values = dict(entries)
        table_address = values.get(5, -1)
        start = next((file_offset + table_address - address for address, file_offset, length in loads if address <= table_address < address + length), None)
        if start is None:
            return None
        source.seek(start)
        strings = source.read(min(values.get(10, 0), 16 << 20))

    def text(index):
        return strings[index:strings.find(b"\0", index)].decode("utf-8", "replace")

    needed = [text(value) for tag, value in entries if tag == 1]
    # DT_RUNPATH replaces DT_RPATH; both apply only to direct dependencies.
    search = [text(value) for tag, value in entries if tag == 29] or [text(value) for tag, value in entries if tag == 15]
    origin = str(path.parent)
    return machine, needed, [part.replace("${ORIGIN}", origin).replace("$ORIGIN", origin) for value in search for part in value.split(":") if part]


def system_directories(cache=Path("/etc/ld.so.cache")):
    try:
        data = cache.read_bytes()
    except OSError:
        data = b""
    configured = sorted({str(Path(match.decode("utf-8", "replace")).parent) for match in re.findall(rb"/[^\0]+\.so[^\0/]*", data)})
    return list(SYSTEM_DIRECTORIES) + [directory for directory in configured if directory not in SYSTEM_DIRECTORIES]


def native_problems(libraries, roots, directories):
    """Resolve the DT_NEEDED closure of the root libraries without loading them.

    Wheels also ship optional plugins for other CUDA, ROCm, MPI or RDMA stacks.
    Only the libraries reachable from the roots count. Missing NVIDIA driver
    libraries are returned separately because only a GPU host has them.
    """
    installed = {path.resolve(): display for display, path in libraries}
    # Torch preloads its bundled CUDA libraries by soname, so a dependency that
    # another installed wheel gives also resolves.
    bundled = {}
    for _, path in sorted(libraries):
        bundled.setdefault(path.name, path.resolve())
    problems, driver = [], []
    pending, visited = deque(sorted(path.resolve() for _, path in roots)), set()
    while pending:
        path = pending.popleft()
        if path in visited:
            continue
        visited.add(path)
        display = installed[path]
        try:
            result = elf_dynamic(path)
        except (OSError, struct.error):
            result = None
        if result is None:
            problems.append(f"{display} is not a readable 64-bit ELF library; reinstall its distribution from the lock")
            continue
        machine, needed, search = result
        for name in needed:
            found = next((candidate for candidate in (Path(directory) / name for directory in [*search, *directories]) if matching_library(candidate, machine)), None)
            if found is None and name in bundled:
                found = bundled[name]
            if found is not None:
                if found.resolve() in installed:
                    pending.append(found.resolve())
            elif name in DRIVER_LIBRARIES:
                driver.append(f"{display} needs the NVIDIA driver library {name}; install the NVIDIA driver on the training host")
            else:
                problems.append(f"{display} needs {name}, which no RUNPATH, installed wheel or system library directory gives; install the system package that gives {name}")
    return problems, driver


def native_roots(libraries, cuda, quantized):
    """Select the Torch libraries and the bitsandbytes library for the Torch CUDA build."""
    roots = [item for item in libraries if item[0].startswith("torch:")]
    problems = []
    if isinstance(cuda, str):
        name = "bitsandbytes:bitsandbytes/libbitsandbytes_cuda" + cuda.replace(".", "") + ".so"
        selected = [item for item in libraries if item[0] == name]
        roots += selected
        if quantized and not selected:
            problems.append(f"bitsandbytes has no library for CUDA {cuda}; install the bitsandbytes wheel from the lock")
    return roots, problems


def matching_library(path, machine):
    try:
        result = elf_dynamic(path) if path.is_file() else None
    except (OSError, struct.error):
        return False
    return result is not None and result[0] == machine


def installed_libraries(distributions):
    libraries = []
    for distribution in distributions:
        name = canonicalize_name(distribution.metadata["Name"])
        for file in distribution.files or ():
            if re.search(r"\.so(\.[0-9]+)*$", file.name):
                libraries.append((f"{name}:{file}", Path(distribution.locate_file(file))))
    return libraries


def dependency_problems(worker):
    try:
        actual = deviations(metadata.distributions())
        if actual != EXPECTED:
            raise ValueError(f"dependency deviations changed: missing={sorted(EXPECTED-actual)}, unexpected={sorted(actual-EXPECTED)}")
        for name, floor in {"python-dotenv": ">=1.2.2", "fsspec": ">=2026.6.0", "pillow": ">=13"}.items():
            if not Requirement(name + floor).specifier.contains(metadata.version(name)):
                raise ValueError("security floor not met: " + name)
        worker.verify_environment()
    except (ValueError, metadata.PackageNotFoundError) as error:
        return [str(error)]
    return []


def accelerator_report(worker, target):
    try:
        facts = worker.torch_facts()
    except (ImportError, OSError) as error:
        message = (str(error).splitlines() or [type(error).__name__])[0][:200].replace(sys.prefix, "<runtime>")
        return {"accelerator": "unavailable"}, [f"Torch cannot load ({message}); reinstall torch and its CUDA wheels from the lock"]
    summary = {key: facts[key] for key in ("torch", "cuda", "driverCuda", "archList", "devices")}
    summary["accelerator"] = "cuda" if facts["available"] else "cpu"
    return summary, worker.accelerator_problems(facts, target == "representative-qlora")


def check(name, problems, target, gpu_only=False):
    # A GPU-only problem does not block the CPU smoke, but stays visible as a warning.
    status = "pass" if not problems else "warn" if gpu_only and target == "cpu-smoke" else "fail"
    return {"name": name, "status": status, "problems": problems[:PROBLEM_LIMIT], "omitted": max(0, len(problems) - PROBLEM_LIMIT)}


def report(target, worker):
    distributions = list(metadata.distributions())
    paths = sysconfig.get_paths()
    stdlib = Path(paths["stdlib"])
    accelerator, accelerator_issues = accelerator_report(worker, target)
    libraries = installed_libraries(distributions)
    roots, root_issues = native_roots(libraries, accelerator.get("cuda"), target == "representative-qlora")
    native_issues, driver_issues = native_problems(libraries, roots, system_directories())
    checks = [
        check("python", python_problems(sys.implementation.name, sys.version_info), target),
        check("runtime-layout", layout_problems(Path(sys.prefix), Path(sys.base_prefix), stdlib), target, gpu_only=True),
        check("standard-library", stdlib_problems([stdlib, Path(paths["platstdlib"])]), target),
        check("dependencies", dependency_problems(worker), target),
        check("locked-wheels", lock_problems(locked_versions(LOCK.read_text(encoding="utf-8")), distributions, set(sys_tags())), target),
        check("wheel-records", record_problems(distributions), target),
        check("native-libraries", root_issues + native_issues, target),
        check("driver-libraries", driver_issues, target, gpu_only=True),
        check("accelerator", accelerator_issues, target, gpu_only=True),
    ]
    return {
        "version": 1, "target": target, "python": ".".join(map(str, sys.version_info[:3])), **accelerator, "checks": checks,
        "status": "rejected" if any(item["status"] == "fail" for item in checks) else "accepted",
    }


def main(args=None):
    parser = argparse.ArgumentParser(description="Check the training runtime for one target.")
    parser.add_argument("--target", choices=TARGETS, default="cpu-smoke")
    target = parser.parse_args(args).target
    path = Path(__file__).resolve().parents[2] / "internal/llmtraining/worker.py"
    spec = importlib.util.spec_from_file_location("training_worker", path)
    worker = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(worker)
    result = report(target, worker)
    print(json.dumps(result, indent=2, sort_keys=True))
    return 1 if result["status"] == "rejected" else 0


if __name__ == "__main__":
    sys.exit(main())
