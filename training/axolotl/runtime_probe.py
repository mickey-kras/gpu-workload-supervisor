"""Load the training runtime natively, in the runner's interpreter order.

check_environment.py starts a private copy of this file as a child process with
-I -S -B and the runner's environment. It writes one JSON object to the result file and reads no dataset.
"""
import ctypes
import importlib
import importlib.util
import json
from pathlib import Path
import sys

# Standard-library modules with native parts that the training stack imports.
STDLIB_MODULES = ("_bz2", "_ctypes", "_decimal", "_hashlib", "_lzma", "_sqlite3", "_ssl", "zlib")


def failure(error):
    lines = str(error).splitlines()
    return type(error).__name__ + (": " + lines[0] if lines else "")


def load_worker(path):
    spec = importlib.util.spec_from_file_location("training_worker", path)
    worker = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(worker)
    return worker


def stdlib_problems(modules=STDLIB_MODULES):
    # The checker finds shadowing copies; this finds native modules that do not load.
    problems = []
    for name in modules:
        try:
            importlib.import_module(name)
        except (ImportError, OSError):
            problems.append(
                f"standard-library module {name} cannot load; rebuild or reinstall CPython with its system libraries"
            )
    return problems


def bitsandbytes_problem(cuda):
    # bitsandbytes selects its library from the Torch CUDA version; load exactly that file, then the package.
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
    try:
        importlib.import_module("bitsandbytes")
    except Exception as error:  # noqa: BLE001 - every import failure is report data.
        return f"bitsandbytes cannot import ({failure(error)}); reinstall bitsandbytes from the lock"
    return None


def probe(worker, quantized):
    sys.path.insert(0, str(worker.runtime_site()))
    result = {"stdlib": stdlib_problems(), "facts": None, "torchError": None, "bitsandbytes": None}
    try:
        result["facts"] = worker.torch_facts()
    except Exception as error:  # noqa: BLE001 - every import or CUDA initialization failure is report data.
        result["torchError"] = failure(error)
    facts = result["facts"]
    if quantized and facts is not None and isinstance(facts["cuda"], str):
        result["bitsandbytes"] = bitsandbytes_problem(facts["cuda"])
    return result


def main(args):
    worker_path, mode, result_path = args
    result = probe(load_worker(worker_path), mode == "quantized")
    Path(result_path).write_text(json.dumps(result), encoding="utf-8")


if __name__ == "__main__":
    main(sys.argv[1:])
