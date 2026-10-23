"""Load the training runtime natively, in the runner's interpreter order.

check_environment.py starts this file as a child process with -I -S -B and the
runner's environment. It prints one JSON object and reads no dataset.
"""
import ctypes
import importlib
import importlib.util
import json
from pathlib import Path
import sys
import sysconfig

# Standard-library modules with native parts that the training stack imports.
STDLIB_MODULES = ("_bz2", "_ctypes", "_decimal", "_hashlib", "_lzma", "_sqlite3", "_ssl", "zlib")


def failure(error):
    lines = str(error).splitlines()
    return type(error).__name__ + (": " + lines[0] if lines else "")


def stdlib_problems(directories, modules=STDLIB_MODULES):
    # Run after the worker's site-packages insertion, so a shadowing copy wins here as it would in the runner.
    problems = []
    for name in modules:
        try:
            spec = importlib.util.find_spec(name)
            importlib.import_module(name)
        except (ImportError, OSError):
            problems.append(
                f"standard-library module {name} cannot load; rebuild or reinstall CPython with its system libraries"
            )
            continue
        origin = spec.origin if spec is not None else None
        inside = origin is not None and any(Path(origin).is_relative_to(path) for path in directories)
        if origin not in {"built-in", "frozen"} and not inside:
            problems.append(
                f"standard-library module {name} does not load from the standard library; remove the shadowing copy"
            )
    return problems


def bitsandbytes_problem(cuda):
    # bitsandbytes selects its library from the Torch CUDA version; load exactly that file.
    spec = importlib.util.find_spec("bitsandbytes")
    if spec is None or spec.origin is None:
        return None
    path = Path(spec.origin).parent / f"libbitsandbytes_cuda{cuda.replace('.', '')}.so"
    if not path.is_file():
        return f"bitsandbytes has no library for CUDA {cuda}; install the bitsandbytes wheel from the lock"
    try:
        ctypes.CDLL(str(path))
    except OSError as error:
        return (
            f"the bitsandbytes CUDA {cuda} library cannot load ({failure(error)}); "
            "reinstall the NVIDIA wheels from the lock"
        )
    return None


def probe(worker, quantized):
    sys.path.insert(0, str(worker.runtime_site()))
    paths = sysconfig.get_paths()
    result = {
        "stdlib": stdlib_problems([Path(paths["stdlib"]), Path(paths["platstdlib"])]),
        "facts": None, "torchError": None, "driverCuda": worker.driver_cuda(), "bitsandbytes": None,
    }
    try:
        result["facts"] = worker.torch_facts()
    except Exception as error:  # noqa: BLE001 - every import or CUDA initialization failure is report data.
        result["torchError"] = failure(error)
    facts = result["facts"]
    if quantized and facts is not None and isinstance(facts["cuda"], str):
        result["bitsandbytes"] = bitsandbytes_problem(facts["cuda"])
    return result


def main(args):
    spec = importlib.util.spec_from_file_location("training_worker", args[0])
    worker = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(worker)
    print(json.dumps(probe(worker, args[1] == "quantized")))


if __name__ == "__main__":
    main(sys.argv[1:])
