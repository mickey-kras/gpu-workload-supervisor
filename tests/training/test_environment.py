"""Guard the subset boundary and exact upstream metadata deviation contract."""
import base64
import hashlib
import importlib.metadata
import importlib.util
import io
import json
from pathlib import Path
import struct
import sys
import sysconfig
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from packaging.tags import Tag
from packaging.version import Version

ROOT = Path(__file__).resolve().parents[2]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


worker = load("subset_worker", "internal/llmtraining/worker.py")
checker = load("subset_checker", "training/axolotl/check_environment.py")


def distribution(name, version="1.0", requires=()):
    return SimpleNamespace(metadata={"Name": name}, version=version, requires=requires)


class SubsetTests(unittest.TestCase):
    def test_each_omission_rejects_installed_metadata_or_shadow_module(self):
        for name, module in worker.OMITTED_INTEGRATIONS.items():
            for metadata_present in (True, False):
                with self.subTest(name=name, metadata_present=metadata_present), patch.object(
                    worker.importlib.metadata, "distributions", return_value=[distribution(name.replace("-", "_"))] if metadata_present else []
                ), patch.object(worker.importlib.util, "find_spec", side_effect=lambda candidate: object() if candidate == module and not metadata_present else None):
                    with self.assertRaisesRegex(ValueError, "unapproved optional"):
                        worker.verify_subset()

    def test_clean_subset_is_accepted_without_importing_optional_code(self):
        with patch.object(worker.importlib.metadata, "distributions", return_value=[]), patch.object(worker.importlib.util, "find_spec", return_value=None):
            worker.verify_subset()

    def test_exact_upstream_deviations(self):
        requires = [name + spec for name, spec in checker.OMITTED.items()] + ["python-dotenv==1.0.1"]
        installed = [distribution("axolotl", requires=requires), distribution("python-dotenv", "1.2.2")]
        self.assertEqual(checker.deviations(installed), checker.EXPECTED)
        for changed in (
            installed + [distribution("trackio", "0.40.0")],
            installed + [distribution("unexpected", requires=["absent>=2"])],
            [distribution("axolotl", requires=requires[:-1])] + installed[1:],
            [distribution("axolotl", requires=requires + ["new-extra>=1"])] + installed[1:],
        ):
            self.assertNotEqual(checker.deviations(changed), checker.EXPECTED)

    def test_active_markers_and_transitive_conflicts_are_checked(self):
        installed = [distribution("parent", requires=["child>=2", "absent; python_version<'3'", "extra-only; extra=='optional'"]), distribution("child", "1")]
        self.assertEqual(checker.deviations(installed), {("parent", "child", ">=2", "version")})
        with self.assertRaisesRegex(ValueError, "duplicate"):
            checker.deviations([distribution("same_name"), distribution("same-name")])

    def test_requested_extras_propagate_through_cycles_and_check_versions(self):
        # Put requesters last so one forward pass cannot discover the closure.
        installed = [
            distribution("leaf", requires=["validator>=2; extra=='validate'", "inactive; extra=='unused'"]),
            distribution("middle", requires=["leaf[validate]; extra=='feature'", "root[again]; extra=='feature'"]),
            distribution("root", requires=["middle[feature]", "middle[feature]; extra=='again'", "never[unused]; python_version<'3'"]),
        ]
        expected = {("leaf", "validator", ">=2", "missing")}
        self.assertEqual(checker.deviations(installed), expected)
        self.assertEqual(checker.deviations(installed + [distribution("validator", "1")]),
                         {("leaf", "validator", ">=2", "version")})
        self.assertEqual(checker.deviations(installed + [distribution("validator", "2")]), set())

    def test_unrequested_extra_does_not_activate_its_dependency_extras(self):
        installed = [
            distribution("root", requires=["middle[feature]; extra=='optional'"]),
            distribution("middle", requires=["missing; extra=='feature'"]),
        ]
        self.assertEqual(checker.deviations(installed), set())


X86_64, AARCH64 = 62, 183


def elf(path, needed=(), runpath=None, machine=X86_64, rpath=None):
    """Write a minimal 64-bit little-endian shared object with a dynamic section."""
    strings, entries = b"\0", []
    for tag, value in [*((1, name) for name in needed), *(((29, runpath),) if runpath else ()), *(((15, rpath),) if rpath else ())]:
        entries.append((tag, len(strings)))
        strings += value.encode() + b"\0"
    dynamic = 64 + 2 * 56
    table = dynamic + 16 * (len(entries) + 3)
    entries += [(5, table), (10, len(strings)), (0, 0)]
    total = table + len(strings)
    header = b"\x7fELF\x02\x01\x01" + bytes(9) + struct.pack("<HHIQQQIHHHHHH", 3, machine, 1, 0, 64, 0, 0, 64, 56, 2, 64, 0, 0)
    programs = struct.pack("<IIQQQQQQ", 1, 4, 0, 0, 0, total, total, 4096) + struct.pack("<IIQQQQQQ", 2, 4, dynamic, dynamic, dynamic, total - dynamic, total - dynamic, 8)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(header + programs + b"".join(struct.pack("<qQ", tag, value) for tag, value in entries) + strings)
    return path


def wheel(name, version="1.0", tag="py3-none-any"):
    return SimpleNamespace(metadata={"Name": name}, version=version, read_text=lambda file: f"Wheel-Version: 1.0\nTag: {tag}\n" if file == "WHEEL" else None)


class RuntimeTests(unittest.TestCase):
    def test_python_must_be_the_locked_cpython_minor(self):
        self.assertEqual(checker.python_problems("cpython", (3, 12, 14)), [])
        self.assertEqual(checker.python_problems("cpython", (3, 12, 3)), [])
        for implementation, version in (("cpython", (3, 13, 1)), ("cpython", (3, 11, 9)), ("pypy", (3, 12, 0))):
            with self.subTest(implementation=implementation, version=version):
                self.assertEqual(checker.python_problems(implementation, version), [
                    f"the runtime is {implementation} {'.'.join(map(str, version))}; the training lock targets CPython 3.12, so install CPython 3.12"])

    def test_runner_layout_needs_a_regular_self_contained_root(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "runtime"
            (root / "bin").mkdir(parents=True)
            (root / "lib/python3.12/site-packages").mkdir(parents=True)
            (root / "bin/python3").write_text("interpreter")
            self.assertEqual(checker.layout_problems(root, root, root / "lib/python3.12"), [])
            (root / "bin/python3").unlink()
            (root / "bin/python3").symlink_to(root / "bin/python3.12")
            self.assertEqual(checker.layout_problems(Path(directory) / "runtime", Path(directory) / "base", Path(directory) / "base/lib/python3.12"), [
                "the runtime is a virtual environment; the runner needs a self-contained CPython root",
                "bin/python3 is not a regular file; copy the interpreter into the runtime root",
                "the standard library is outside the runtime root; install it into the same root",
            ])
            (root / "lib/python3.12/site-packages").rmdir()
            self.assertIn("lib/python3.12/site-packages is not a regular directory in the runtime root", checker.layout_problems(root, root, root / "lib/python3.12"))

    def test_standard_library_modules_load_from_the_standard_library(self):
        paths = sysconfig.get_paths()
        directories = [Path(paths["stdlib"]), Path(paths["platstdlib"])]
        self.assertEqual(checker.stdlib_problems(directories, ("json", "sys")), [])
        self.assertEqual(checker.stdlib_problems(directories, ("absent_stdlib_module",)), [
            "standard-library module absent_stdlib_module cannot load; rebuild or reinstall CPython with its system libraries"])
        with tempfile.TemporaryDirectory() as directory, patch.object(sys, "path", [directory, *sys.path]):
            (Path(directory) / "shadow_stdlib_module.py").write_text("")
            self.assertEqual(checker.stdlib_problems(directories, ("shadow_stdlib_module",)), [
                "standard-library module shadow_stdlib_module is outside the standard library; remove the shadowing copy"])
            sys.modules.pop("shadow_stdlib_module", None)
        with patch.object(checker.importlib, "import_module", side_effect=OSError("libssl.so.3")):
            self.assertEqual(checker.stdlib_problems(directories, ("json",)), [
                "standard-library module json cannot load; rebuild or reinstall CPython with its system libraries"])

    def test_locked_versions_read_hash_locked_requirements(self):
        text = "Torch==2.14.0 \\\n    --hash=sha256:abc\nnvidia_cublas==13.1.1.3 \\\n    --hash=sha256:def\n# comment==1\nmarker==1.0 ; sys_platform != 'darwin'\n"
        self.assertEqual(checker.locked_versions(text), {"torch": Version("2.14.0"), "nvidia-cublas": Version("13.1.1.3"), "marker": Version("1.0")})
        locked = checker.locked_versions(checker.LOCK.read_text(encoding="utf-8"))
        self.assertEqual(locked["torch"], Version("2.14.0"))
        self.assertNotIn("axolotl", locked)

    def test_installed_wheels_match_the_lock_exactly(self):
        supported = {Tag("py3", "none", "any"), Tag("cp312", "cp312", "manylinux_2_28_x86_64")}
        locked = {"torch": Version("2.14.0"), "peft": Version("0.21.2")}
        exact = [wheel("torch", "2.14.0", "cp312-cp312-manylinux_2_28_x86_64"), wheel("PEFT", "0.21.2"), wheel("axolotl", "0.21.0")]
        self.assertEqual(checker.lock_problems(locked, exact, supported), [])
        changed = [wheel("torch", "2.13.0", "cp311-cp311-manylinux_2_28_x86_64"), wheel("unlocked_extra"), wheel("axolotl", "0.21.0")]
        self.assertEqual(checker.lock_problems(locked, changed, supported), [
            "peft==0.21.2 from the lock is missing; run uv pip sync --require-hashes with the lock",
            "unlocked-extra is not in the lock; remove it from the training runtime",
            "torch 2.13.0 differs from the locked 2.14.0; run uv pip sync with the lock",
            "torch is not an installed wheel for this interpreter and platform; reinstall it from the lock",
        ])
        missing_wheel = SimpleNamespace(metadata={"Name": "peft"}, version="0.21.2", read_text=lambda file: None)
        self.assertEqual(checker.lock_problems({"peft": Version("0.21.2")}, [missing_wheel], supported), [
            "peft is not an installed wheel for this interpreter and platform; reinstall it from the lock"])

    def test_installed_files_match_their_record(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "package").mkdir()
            info = root / "package-1.0.dist-info"
            info.mkdir()
            (info / "METADATA").write_text("Metadata-Version: 2.1\nName: package\nVersion: 1.0\n")
            rows = []
            for name, data in (("package/a.py", b"first"), ("package/b.so", b"second"), ("package/c.py", b"third")):
                (root / name).write_bytes(data)
                digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
                rows.append(f"{name},sha256={digest},{len(data)}")
            rows.append("package-1.0.dist-info/RECORD,,")
            (info / "RECORD").write_text("\n".join(rows) + "\n")
            distribution = importlib.metadata.PathDistribution(info)
            self.assertEqual(checker.record_problems([distribution]), [])
            (root / "package/a.py").write_bytes(b"FIRST")
            (root / "package/b.so").write_bytes(b"longer bytes")
            (root / "package/c.py").unlink()
            self.assertEqual(checker.record_problems([distribution]), [
                f"package file package/{name} differs from its RECORD; reinstall package from the lock" for name in ("a.py", "b.so", "c.py")])
            libraries = checker.installed_libraries([distribution])
            self.assertEqual(libraries, [("package:package/b.so", root / "package/b.so")])

    def test_elf_dynamic_reads_needed_libraries_and_search_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = elf(root / "lib/libroot.so", ["libc.so.6", "libdep.so.1"], runpath="$ORIGIN/../deps:${ORIGIN}", rpath="/ignored")
            self.assertEqual(checker.elf_dynamic(path), (X86_64, ["libc.so.6", "libdep.so.1"], [f"{root}/lib/../deps", f"{root}/lib"]))
            path = elf(root / "lib/librpath.so", ["libc.so.6"], rpath="/opt/lib", machine=AARCH64)
            self.assertEqual(checker.elf_dynamic(path), (AARCH64, ["libc.so.6"], ["/opt/lib"]))
            (root / "script.so").write_text("#!/bin/sh\n")
            self.assertIsNone(checker.elf_dynamic(root / "script.so"))
            static = bytearray(elf(root / "static.so").read_bytes())
            struct.pack_into("<I", static, 64 + 56, 4)
            (root / "static.so").write_bytes(bytes(static))
            self.assertEqual(checker.elf_dynamic(root / "static.so"), (X86_64, [], []))
            broken = bytearray(elf(root / "broken.so", ["libc.so.6"]).read_bytes())
            struct.pack_into("<Q", broken, 64 + 32, 8)
            (root / "broken.so").write_bytes(bytes(broken))
            self.assertIsNone(checker.elf_dynamic(root / "broken.so"))

    def test_native_closure_resolves_through_runpath_wheels_and_system_directories(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            site, system = root / "site-packages", root / "system"
            elf(system / "libc.so.6")
            elf(system / "libwrong.so.1", machine=AARCH64)
            root_library = elf(site / "torch/lib/libtorch.so", ["libc.so.6", "libcudart.so.13", "libcublas.so.13", "libcuda.so.1"], runpath="$ORIGIN/../../nvidia/lib")
            elf(site / "nvidia/lib/libcudart.so.13", ["libc.so.6"])
            elf(site / "nvidia/lib/libcublas.so.13", ["libnvjitlink.so.13", "libwrong.so.1", "libabsent.so.2"])
            elf(site / "nvidia/other/libnvjitlink.so.13", ["libc.so.6"])
            elf(site / "nvidia/plugins/librdma.so.1", ["libibverbs.so.1"])
            (site / "torch/lib/libbroken.so").write_text("not an ELF file")
            libraries = [(f"wheel:{path.relative_to(site)}", path) for path in sorted(site.rglob("*.so*"))]
            roots = [("wheel:torch/lib/libtorch.so", root_library), ("wheel:torch/lib/libbroken.so", site / "torch/lib/libbroken.so")]
            problems, driver = checker.native_problems(libraries, roots, [str(system)])
            self.assertEqual(problems, [
                "wheel:torch/lib/libbroken.so is not a readable 64-bit ELF library; reinstall its distribution from the lock",
                "wheel:nvidia/lib/libcublas.so.13 needs libwrong.so.1, which no RUNPATH, installed wheel or system library directory gives; install the system package that gives libwrong.so.1",
                "wheel:nvidia/lib/libcublas.so.13 needs libabsent.so.2, which no RUNPATH, installed wheel or system library directory gives; install the system package that gives libabsent.so.2",
            ])
            self.assertEqual(driver, ["wheel:torch/lib/libtorch.so needs the NVIDIA driver library libcuda.so.1; install the NVIDIA driver on the training host"])
            elf(system / "libcuda.so.1")
            elf(system / "libwrong.so.1")
            elf(system / "libabsent.so.2")
            self.assertEqual(checker.native_problems(libraries, roots[:1], [str(system)]), ([], []))

    def test_native_roots_select_torch_and_the_matching_bitsandbytes_library(self):
        libraries = [(name, Path("/runtime") / name) for name in (
            "torch:torch/lib/libtorch.so", "torch:torch/_C.cpython-312-x86_64-linux-gnu.so",
            "bitsandbytes:bitsandbytes/libbitsandbytes_cuda128.so", "bitsandbytes:bitsandbytes/libbitsandbytes_cuda130.so",
            "numba:numba/np/ufunc/tbbpool.so")]
        self.assertEqual(checker.native_roots(libraries, "13.0", True), (libraries[:2] + [libraries[3]], []))
        self.assertEqual(checker.native_roots(libraries, None, True), (libraries[:2], []))
        self.assertEqual(checker.native_roots(libraries, "13.2", False), (libraries[:2], []))
        self.assertEqual(checker.native_roots(libraries, "13.2", True), (libraries[:2], [
            "bitsandbytes has no library for CUDA 13.2; install the bitsandbytes wheel from the lock"]))

    def test_system_directories_add_loader_cache_entries(self):
        with tempfile.TemporaryDirectory() as directory:
            cache = Path(directory) / "ld.so.cache"
            cache.write_bytes(b"glibc-ld.so.cache1.1\0libc.so.6\0/usr/lib/x86_64-linux-gnu/libc.so.6\0/opt/vendor/lib/libvendor.so.2\0/lib\0")
            self.assertEqual(checker.system_directories(cache), [*checker.SYSTEM_DIRECTORIES, "/opt/vendor/lib", "/usr/lib/x86_64-linux-gnu"])
            self.assertEqual(checker.system_directories(Path(directory) / "missing"), list(checker.SYSTEM_DIRECTORIES))

    def test_check_status_keeps_gpu_problems_visible_for_the_cpu_smoke(self):
        problems = [str(index) for index in range(checker.PROBLEM_LIMIT + 2)]
        self.assertEqual(checker.check("accelerator", [], "smoke-lora", gpu_only=True)["status"], "pass")
        self.assertEqual(checker.check("accelerator", ["no GPU"], "cpu-smoke", gpu_only=True)["status"], "warn")
        self.assertEqual(checker.check("accelerator", ["no GPU"], "representative-qlora", gpu_only=True)["status"], "fail")
        self.assertEqual(checker.check("python", problems, "cpu-smoke"), {
            "name": "python", "status": "fail", "problems": problems[:checker.PROBLEM_LIMIT], "omitted": 2})

    def test_accelerator_report_is_explicit_about_cpu_and_load_failures(self):
        cpu = {"torch": "2.14.0", "cuda": "13.0", "hip": None, "available": False, "driverCuda": None, "archList": [], "devices": [], "bitsandbytes": True}
        stub = SimpleNamespace(torch_facts=lambda: cpu, accelerator_problems=worker.accelerator_problems)
        summary, problems = checker.accelerator_report(stub, "cpu-smoke")
        self.assertEqual(summary, {"torch": "2.14.0", "cuda": "13.0", "driverCuda": None, "archList": [], "devices": [], "accelerator": "cpu"})
        self.assertEqual(problems, ["Torch CUDA 13.0 cannot use a GPU; install an NVIDIA driver for CUDA 13.0 or newer and expose one GPU"])

        def fail():
            raise OSError(f"{sys.prefix}/lib/libcudnn.so.9: cannot open shared object file\nsecond line")

        summary, problems = checker.accelerator_report(SimpleNamespace(torch_facts=fail), "smoke-lora")
        self.assertEqual(summary, {"accelerator": "unavailable"})
        self.assertEqual(problems, ["Torch cannot load (<runtime>/lib/libcudnn.so.9: cannot open shared object file); reinstall torch and its CUDA wheels from the lock"])

    def test_dependency_problems_report_the_existing_contract(self):
        stub = SimpleNamespace(verify_environment=lambda: None)
        with patch.object(checker, "deviations", return_value=set()):
            self.assertEqual(len(checker.dependency_problems(stub)), 1)
            self.assertIn("dependency deviations changed", checker.dependency_problems(stub)[0])
        versions = {"python-dotenv": "1.2.2", "fsspec": "2026.6.0", "pillow": "13.0"}
        with patch.object(checker, "deviations", return_value=checker.EXPECTED), patch.object(checker.metadata, "version", side_effect=versions.get):
            self.assertEqual(checker.dependency_problems(stub), [])
            versions["pillow"] = "12.0"
            self.assertEqual(checker.dependency_problems(stub), ["security floor not met: pillow"])

    def test_main_prints_a_report_and_fails_closed(self):
        for status, code in (("accepted", 0), ("rejected", 1)):
            with self.subTest(status=status), patch.object(checker, "report", return_value={"status": status}) as report, patch("sys.stdout", new_callable=io.StringIO) as output:
                self.assertEqual(checker.main(["--target", "representative-qlora"]), code)
                self.assertEqual(json.loads(output.getvalue()), {"status": status})
                self.assertEqual(report.call_args.args[0], "representative-qlora")
        with patch("sys.stderr", new_callable=io.StringIO), self.assertRaises(SystemExit):
            checker.main(["--target", "unknown"])

    def test_report_runs_every_check_for_the_current_interpreter(self):
        stub = SimpleNamespace(verify_environment=lambda: None, accelerator_problems=worker.accelerator_problems, torch_facts=lambda: {
            "torch": "2.14.0", "cuda": None, "hip": None, "available": False, "driverCuda": None, "archList": [], "devices": [], "bitsandbytes": False})
        with patch.object(checker.metadata, "distributions", return_value=[]), patch.object(checker, "dependency_problems", return_value=[]):
            result = checker.report("smoke-lora", stub)
        self.assertEqual([item["name"] for item in result["checks"]], [
            "python", "runtime-layout", "standard-library", "dependencies", "locked-wheels", "wheel-records",
            "native-libraries", "driver-libraries", "accelerator"])
        self.assertEqual((result["version"], result["target"], result["accelerator"], result["status"]), (1, "smoke-lora", "cpu", "rejected"))
        self.assertEqual(next(item for item in result["checks"] if item["name"] == "accelerator")["problems"], [
            "Torch 2.14.0 is not a CUDA build; install the CUDA torch wheel from the training lock"])


if __name__ == "__main__":
    unittest.main()
