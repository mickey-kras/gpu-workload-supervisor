"""Guard the subset boundary, the exact deviation contract and the runtime report."""
import hashlib
import importlib.metadata
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import sysconfig
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from packaging.version import Version

ROOT = Path(__file__).resolve().parents[2]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


worker = load("subset_worker", "internal/llmtraining/worker.py")
checker = load("subset_checker", "training/axolotl/check_environment.py")
probe = load("runtime_probe", "training/axolotl/runtime_probe.py")


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


def wheel(name, version="1.0", files=()):
    return SimpleNamespace(metadata={"Name": name}, version=version, files=[Path(file) for file in files])


def cuda_facts(**changes):
    facts = {
        "torch": "2.14.0", "cuda": "13.0", "hip": None, "available": True, "driverCuda": "13.0",
        "archList": ["sm_80", "sm_86", "sm_90", "sm_120"], "devices": [[8, 6]], "bitsandbytes": True,
    }
    facts.update(changes)
    return facts


def probe_result(**changes):
    result = {"stdlib": [], "facts": cuda_facts(), "torchError": None, "driverCuda": "13.0", "bitsandbytes": None}
    result.update(changes)
    return result


def fake_run(result=None, code=0, stderr=b"", stdout=None):
    """Return a subprocess.run replacement that writes the probe output."""
    calls = []

    def run(command, **options):
        calls.append((command, options))
        options["stdout"].write(stdout if stdout is not None else (b"torch noise\n" + json.dumps(result).encode() + b"\n"))
        options["stderr"].write(stderr)
        return SimpleNamespace(returncode=code)

    return run, calls


class RuntimeTests(unittest.TestCase):
    def test_targets_come_from_the_asset_presets(self):
        presets = json.loads((ROOT / "training/assets.json").read_text())
        self.assertEqual(checker.TARGETS, ("cpu-smoke", *presets))
        self.assertLessEqual(checker.QUANTIZED, set(checker.TARGETS))
        self.assertNotIn("cpu-smoke", checker.QUANTIZED)

    def test_runner_layout_needs_a_regular_self_contained_root(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "runtime"
            site = root / "lib" / f"python{sys.version_info.major}.{sys.version_info.minor}" / "site-packages"
            (root / "bin").mkdir(parents=True)
            site.mkdir(parents=True)
            (root / "bin/python3").write_text("interpreter")
            self.assertEqual(checker.layout_problems(root, root, site.parent), [])
            (root / "bin/python3").unlink()
            (root / "bin/python3").symlink_to(root / "bin/python3.12")
            self.assertEqual(checker.layout_problems(root, Path(directory) / "base", Path(directory) / "base/lib"), [
                "the runtime is a virtual environment; the runner needs a self-contained CPython root",
                "bin/python3 is not a regular file; copy the interpreter into the runtime root",
                "the standard library is outside the runtime root; install it into the same root",
            ])
            site.rmdir()
            self.assertIn(f"{site.relative_to(root)} is not a regular directory in the runtime root",
                          checker.layout_problems(root, root, site.parent))

    def test_site_packages_must_not_shadow_or_customize_the_standard_library(self):
        with tempfile.TemporaryDirectory() as directory:
            site = Path(directory)
            for name in ("json.py", "sitecustomize.py", "usercustomize.pyc", "_ssl.cpython-312-x86_64-linux-gnu.so",
                         "torch/", "email/", "torch-2.14.0.dist-info/", "owned.pth", "stray.pth", "README.txt"):
                if name.endswith("/"):
                    (site / name).mkdir()
                else:
                    (site / name).write_text("")
            shadows, startup = checker.site_problems(site, {"owned.pth"})
            self.assertEqual(shadows, [
                "site-packages has _ssl.cpython-312-x86_64-linux-gnu.so, which shadows the standard-library module _ssl; remove it",
                "site-packages has email, which shadows the standard-library module email; remove it",
                "site-packages has json.py, which shadows the standard-library module json; remove it",
                "site-packages has sitecustomize.py; remove the startup customization",
                "site-packages has usercustomize.pyc; remove the startup customization",
            ])
            self.assertEqual(startup, ["site-packages has the startup file stray.pth, which no locked wheel installs; remove it"])
            self.assertEqual(checker.site_problems(site / "missing", set()), ([], []))

    def test_only_startup_files_of_locked_wheels_are_owned(self):
        distributions = [
            wheel("coloredlogs", files=["coloredlogs.pth", "coloredlogs/__init__.py"]),
            wheel("unlocked", files=["unlocked.pth"]),
            wheel("setuptools", files=["setuptools/nested.pth", "distutils-precedence.pth"]),
        ]
        self.assertEqual(checker.owned_startup_files(distributions, {"coloredlogs": Version("1"), "setuptools": Version("1")}),
                         {"coloredlogs.pth", "distutils-precedence.pth"})

    def test_locked_versions_read_hash_locked_requirements(self):
        text = "Torch==2.14.0 \\\n    --hash=sha256:abc\nnvidia_cublas==13.1.1.3 \\\n    --hash=sha256:def\n# comment==1\nmarker==1.0 ; sys_platform != 'darwin'\n"
        self.assertEqual(checker.locked_versions(text), {"torch": Version("2.14.0"), "nvidia-cublas": Version("13.1.1.3"), "marker": Version("1.0")})
        locked = checker.locked_versions(checker.LOCK.read_text(encoding="utf-8"))
        self.assertEqual(locked["torch"], Version("2.14.0"))
        self.assertNotIn("axolotl", locked)

    def test_installed_distributions_match_the_lock_exactly(self):
        locked = {"torch": Version("2.14.0"), "peft": Version("0.21.2")}
        self.assertEqual(checker.lock_problems(locked, [wheel("torch", "2.14.0"), wheel("PEFT", "0.21.2"), wheel("axolotl", "0.21.0")]), [])
        self.assertEqual(checker.lock_problems(locked, [wheel("torch", "2.13.0"), wheel("unlocked_extra"), wheel("axolotl")]), [
            "peft==0.21.2 from the lock is missing; run uv pip sync --require-hashes with the lock",
            "unlocked-extra is not in the lock; run uv pip sync --require-hashes with the lock to remove it",
            "torch 2.13.0 differs from the locked 2.14.0; run uv pip sync --require-hashes with the lock",
        ])

    def test_dependency_problems_give_a_remedy(self):
        revision = worker.SOURCE_REVISION
        sync = "reinstall the training runtime from the lock with uv pip sync --require-hashes"
        versions = {"python-dotenv": "1.2.2", "fsspec": "2026.6.0", "pillow": "13.0"}

        def raising(error):
            def verify():
                raise error
            return SimpleNamespace(SOURCE_REVISION=revision, verify_environment=verify)

        with patch.object(checker, "deviations", return_value=set()):
            problems = checker.dependency_problems(raising(None))
            self.assertEqual(len(problems), 1)
            self.assertTrue(problems[0].startswith("dependency deviations changed: missing="))
            self.assertTrue(problems[0].endswith(sync))
        with patch.object(checker, "deviations", return_value=checker.EXPECTED), patch.object(checker.metadata, "version", side_effect=versions.get):
            self.assertEqual(checker.dependency_problems(SimpleNamespace(SOURCE_REVISION=revision, verify_environment=lambda: None)), [])
            for error, expected in (
                (ValueError("runtime source is not pinned"), f"runtime source is not pinned; install axolotl noneditable with --no-deps from commit {revision}"),
                (ValueError("unapproved runtime entry point"), "unapproved runtime entry point; remove the distribution that registers it"),
                (ValueError("unapproved optional runtime integration: gradio"),
                 "unapproved optional runtime integration: gradio; run uv pip sync --require-hashes with the lock to remove it"),
                (importlib.metadata.PackageNotFoundError("axolotl"), f"axolotl is not installed; install axolotl noneditable with --no-deps from commit {revision}"),
            ):
                with self.subTest(error=error):
                    self.assertEqual(checker.dependency_problems(raising(error)), [expected])
            versions["pillow"] = "12.0"
            self.assertEqual(checker.dependency_problems(raising(None)), [f"security floor not met: pillow; {sync}"])

    def test_probe_environment_mirrors_the_runner_and_drops_loader_variables(self):
        work = Path("/work")
        with patch.dict(os.environ, {"LD_LIBRARY_PATH": "/injected", "LD_PRELOAD": "/injected.so", "CUDA_VISIBLE_DEVICES": "1"}):
            environment = checker.probe_environment(work, None)
        self.assertEqual(set(environment), {
            "PATH", "HOME", "XDG_CONFIG_HOME", "TMPDIR", "HF_HOME", "HF_HUB_OFFLINE", "HF_DATASETS_OFFLINE",
            "TRANSFORMERS_OFFLINE", "HF_HUB_DISABLE_TELEMETRY", "AXOLOTL_DO_NOT_TRACK", "DO_NOT_TRACK",
            "WANDB_MODE", "TOKENIZERS_PARALLELISM", "LANG"})
        self.assertEqual(environment["PATH"], f"{Path(sys.executable).parent}:/usr/bin:/bin")
        self.assertEqual(checker.probe_environment(work, "GPU-1234")["CUDA_VISIBLE_DEVICES"], "GPU-1234")
        runner = (ROOT / "internal/llmtraining/run.go").read_text()
        for name in environment:
            self.assertIn(f'"{name}=', runner)
        self.assertIn('"CUDA_VISIBLE_DEVICES=', runner)

    def test_run_probe_uses_a_runner_like_child_and_bounded_output(self):
        run, calls = fake_run(probe_result())
        self.assertEqual(checker.run_probe(True, "GPU-1", run), (probe_result(), None))
        command, options = calls[0]
        self.assertEqual(command, [sys.executable, "-I", "-S", "-B", str(checker.PROBE), str(checker.WORKER), "quantized"])
        self.assertEqual((options["env"]["CUDA_VISIBLE_DEVICES"], options["timeout"], options["check"]), ("GPU-1", checker.PROBE_TIMEOUT, False))
        self.assertEqual(options["cwd"], Path(options["env"]["TMPDIR"]))
        self.assertFalse(Path(options["env"]["TMPDIR"]).exists())
        retry = "reinstall the training runtime from the lock and run the check again"
        for run, expected in (
            (fake_run(code=1, stderr=b"Traceback\nImportError: libtorch_cuda.so: cannot open\n\n")[0],
             f"the runtime probe failed (ImportError: libtorch_cuda.so: cannot open); {retry}"),
            (fake_run(code=-9, stdout=b"")[0], f"the runtime probe failed (exit status -9); {retry}"),
            (fake_run(stdout=b"")[0], f"the runtime probe gave no valid result; {retry}"),
            (fake_run(stdout=b"[1]\n")[0], f"the runtime probe gave no valid result; {retry}"),
            (fake_run(stdout=b"{broken\n")[0], f"the runtime probe gave no valid result; {retry}"),
        ):
            with self.subTest(expected=expected):
                self.assertEqual(checker.run_probe(False, None, run), (None, expected))

        def timeout(command, **options):
            raise subprocess.TimeoutExpired(command, checker.PROBE_TIMEOUT)

        self.assertEqual(checker.run_probe(False, None, timeout),
                         (None, f"the runtime probe did not finish in {checker.PROBE_TIMEOUT} seconds; {retry}"))

    def test_failing_child_process_maps_to_an_actionable_problem(self):
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / "probe.py"
            script.write_text("import sys\nsys.stderr.write('OSError: libcudnn.so.9: cannot open shared object file\\n')\nsys.exit(3)\n")
            with patch.object(checker, "PROBE", script):
                probe, failure = checker.run_probe(False, None)
        self.assertIsNone(probe)
        self.assertEqual(failure, "the runtime probe failed (OSError: libcudnn.so.9: cannot open shared object file); "
                                  "reinstall the training runtime from the lock and run the check again")
        problems = checker.probe_checks(None, failure, "cpu-smoke", None, {})
        self.assertEqual(problems["native"], [failure])
        self.assertEqual(problems["accelerator"], ["the runtime probe did not run; fix the native-libraries problem first"])

    def test_real_probe_child_runs_isolated_with_a_fake_worker(self):
        with tempfile.TemporaryDirectory() as directory:
            fake = Path(directory) / "worker.py"
            fake.write_text(
                "import os, sys\nfrom pathlib import Path\n"
                "def runtime_site():\n    return Path(os.environ['TMPDIR']) / 'site'\n"
                "def driver_cuda():\n    return None if 'CUDA_VISIBLE_DEVICES' not in os.environ else '13.0'\n"
                "def torch_facts():\n    flags = (sys.flags.isolated, sys.flags.no_site, sys.flags.dont_write_bytecode)\n"
                "    raise ImportError('no torch ' + repr(flags) + ' ' + str('LD_PRELOAD' in os.environ))\n")
            with patch.object(checker, "WORKER", fake), patch.dict(os.environ, {"LD_PRELOAD": "/injected.so"}):
                probe, failure = checker.run_probe(True, "GPU-1")
        self.assertIsNone(failure)
        self.assertEqual(probe, {"stdlib": [], "facts": None, "torchError": "ImportError: no torch (1, 1, 1) False",
                                 "driverCuda": "13.0", "bitsandbytes": None})

    def test_probe_results_map_to_check_problems(self):
        cases = (
            (probe_result(), "smoke-lora", "GPU-1", {}, {"stdlib": [], "native": [], "driver": [], "accelerator": []}),
            (probe_result(torchError="OSError: libcudnn.so.9", facts=None, driverCuda=None), "smoke-lora", None, {}, {
                "stdlib": [], "native": ["Torch cannot load (OSError: libcudnn.so.9); reinstall torch and its CUDA wheels from the lock"],
                "driver": ["the NVIDIA driver library libcuda.so.1 does not load; install the NVIDIA driver"],
                "accelerator": ["no trainer GPU is selected; pass --gpu-uuid with the trainer GPU UUID",
                                "the accelerator check needs Torch; fix the native-libraries problem first"]}),
            (probe_result(bitsandbytes="bitsandbytes has no library for CUDA 13.0", stdlib=["shadow"]), "representative-qlora", "GPU-1",
             {"BNB_CUDA_VERSION": "128"}, {
                "stdlib": ["shadow"], "native": ["bitsandbytes has no library for CUDA 13.0",
                                                 "BNB_CUDA_VERSION is set, which the training runtime does not support; unset it"],
                "driver": [], "accelerator": []}),
            (probe_result(facts=cuda_facts(bitsandbytes=False)), "representative-qlora", "GPU-1", {}, {
                "stdlib": [], "native": [], "driver": [], "accelerator": ["QLoRA needs bitsandbytes; install bitsandbytes from the training lock"]}),
            (probe_result(), "smoke-lora", "GPU-1", {"BNB_CUDA_VERSION": "128"}, {"stdlib": [], "native": [], "driver": [], "accelerator": []}),
        )
        for probe, target, gpu, environment, expected in cases:
            with self.subTest(target=target, probe=probe):
                self.assertEqual(checker.probe_checks(probe, None, target, gpu, environment), expected)

    def test_check_status_bounds_and_masks_problems(self):
        problems = [str(index) for index in range(checker.PROBLEM_LIMIT + 2)]
        self.assertEqual(checker.check("accelerator", [], "smoke-lora", cpu_smoke_warning=True)["status"], "pass")
        self.assertEqual(checker.check("accelerator", ["no GPU"], "cpu-smoke", cpu_smoke_warning=True)["status"], "warn")
        for target in checker.PRESETS:
            self.assertEqual(checker.check("runtime-layout", ["no GPU"], target, cpu_smoke_warning=True)["status"], "fail")
        self.assertEqual(checker.check("python", problems, "cpu-smoke"), {
            "name": "python", "status": "fail", "problems": problems[:checker.PROBLEM_LIMIT], "omitted": 2})
        long = checker.check("python", [f"x {sys.prefix}/lib/a.so and /opt/secret/b.so in lib/c.so " + "y" * 400], "cpu-smoke")
        self.assertTrue(long["problems"][0].startswith("x <runtime>/lib/a.so and <path> in lib/c.so y"))
        self.assertEqual(len(long["problems"][0]), checker.PROBLEM_LENGTH)

    def test_a_broken_check_is_a_failure_entry(self):
        def broken():
            raise KeyError("facts")

        self.assertEqual(checker.guarded("accelerator", broken, "cpu-smoke", cpu_smoke_warning=True), {
            "name": "accelerator", "status": "fail", "problems": ["check could not run: KeyError"], "omitted": 0})

    def report(self, target, probe, gpu_uuid="GPU-1", clean=True):
        run, _ = fake_run(probe)
        patches = [patch.object(checker.metadata, "distributions", return_value=[])]
        if clean:
            patches += [patch.object(checker, "lock_problems", return_value=[]), patch.object(checker, "dependency_problems", return_value=[]),
                        patch.object(checker, "site_problems", return_value=([], []))]
        for item in patches:
            item.start()
        try:
            return checker.report(target, gpu_uuid, run)
        finally:
            for item in patches:
                item.stop()

    def test_report_keeps_stable_keys_and_accepts_a_clean_cpu_smoke(self):
        cpu = probe_result(facts=cuda_facts(available=False, archList=[], devices=[], driverCuda=None), driverCuda=None)
        result = self.report("cpu-smoke", cpu, gpu_uuid=None)
        self.assertEqual(set(result), {"version", "target", "status", "python", "lockTarget", "workerSHA256", "lockSHA256",
                                       "accelerator", "torch", "cuda", "driverCuda", "archList", "devices", "checks"})
        self.assertEqual((result["version"], result["status"], result["accelerator"], result["lockTarget"]), (1, "accepted", "cpu", checker.LOCK_TARGET))
        self.assertEqual(result["workerSHA256"], hashlib.sha256(checker.WORKER.read_bytes()).hexdigest())
        self.assertEqual(result["lockSHA256"], hashlib.sha256(checker.LOCK.read_bytes()).hexdigest())
        statuses = {item["name"]: item["status"] for item in result["checks"]}
        self.assertEqual(list(statuses), ["python", "runtime-layout", "standard-library", "startup-files", "dependencies",
                                          "locked-versions", "native-libraries", "driver-libraries", "accelerator"])
        self.assertEqual((statuses["driver-libraries"], statuses["accelerator"]), ("warn", "warn"))
        self.assertTrue(all(status in {"pass", "warn"} for status in statuses.values()))

    def test_report_rejects_presets_with_gpu_only_problems(self):
        result = self.report("smoke-lora", probe_result(driverCuda=None))
        self.assertEqual(result["status"], "rejected")
        self.assertEqual(next(item for item in result["checks"] if item["name"] == "driver-libraries")["status"], "fail")
        result = self.report("representative-qlora", probe_result(facts=cuda_facts(bitsandbytes=False)))
        accelerator = next(item for item in result["checks"] if item["name"] == "accelerator")
        self.assertEqual((result["status"], result["accelerator"], accelerator["status"]), ("rejected", "cuda", "fail"))
        self.assertEqual(accelerator["problems"], ["QLoRA needs bitsandbytes; install bitsandbytes from the training lock"])

    def test_report_without_a_probe_result_has_null_runtime_fields(self):
        result = self.report("cpu-smoke", None)
        self.assertEqual([result[key] for key in ("accelerator", "torch", "cuda", "driverCuda", "archList", "devices")], [None] * 6)
        self.assertEqual(result["status"], "rejected")

        def explode(*args, **options):
            raise OSError("no temporary directory")

        with patch.object(checker.metadata, "distributions", return_value=[]):
            result = checker.report("cpu-smoke", None, explode)
        native = next(item for item in result["checks"] if item["name"] == "native-libraries")
        self.assertEqual(native["problems"], ["the runtime probe could not run: OSError"])

    def test_main_exit_codes(self):
        for status, code in (("accepted", 0), ("rejected", 1)):
            with self.subTest(status=status), patch.object(checker, "report", return_value={"status": status}) as report, \
                    patch("sys.stdout", new_callable=io.StringIO) as output:
                self.assertEqual(checker.main(["--target", "representative-qlora", "--gpu-uuid", "GPU-0a1b"]), code)
                self.assertEqual(json.loads(output.getvalue()), {"status": status})
                self.assertEqual(report.call_args.args, ("representative-qlora", "GPU-0a1b"))
        with patch.object(checker, "report", side_effect=RuntimeError("CUDA init")), patch("sys.stdout", new_callable=io.StringIO) as output:
            self.assertEqual(checker.main([]), 2)
            self.assertEqual(json.loads(output.getvalue()), {"version": 1, "target": "cpu-smoke", "status": "rejected",
                                                             "error": "check could not run: RuntimeError"})
        for arguments in (["--target", "unknown"], ["--gpu-uuid", "0"], ["--gpu-uuid", "GPU-1;rm"]):
            with self.subTest(arguments=arguments), patch("sys.stderr", new_callable=io.StringIO), self.assertRaises(SystemExit) as exit:
                checker.main(arguments)
            self.assertEqual(exit.exception.code, 2)

    def test_check_writes_no_bytecode(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = Path(importlib.util.find_spec("packaging").origin).parent
            shutil.copytree(source, root / "packaging", ignore=shutil.ignore_patterns("__pycache__"))
            before = sorted(path.relative_to(root) for path in root.rglob("*"))
            environment = {**os.environ, "PYTHONPATH": directory}
            environment.pop("PYTHONDONTWRITEBYTECODE", None)
            completed = subprocess.run([sys.executable, str(ROOT / "training/axolotl/check_environment.py"), "--help"],
                                       env=environment, capture_output=True, check=False)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertEqual(sorted(path.relative_to(root) for path in root.rglob("*")), before)


class ProbeTests(unittest.TestCase):
    def test_standard_library_modules_load_from_the_standard_library(self):
        paths = sysconfig.get_paths()
        directories = [Path(paths["stdlib"]), Path(paths["platstdlib"])]
        self.assertEqual(probe.stdlib_problems(directories, ("json", "sys")), [])
        self.assertEqual(probe.stdlib_problems(directories, ("absent_stdlib_module",)), [
            "standard-library module absent_stdlib_module cannot load; rebuild or reinstall CPython with its system libraries"])
        with tempfile.TemporaryDirectory() as directory, patch.object(sys, "path", [directory, *sys.path]):
            (Path(directory) / "shadow_stdlib_module.py").write_text("")
            self.assertEqual(probe.stdlib_problems(directories, ("shadow_stdlib_module",)), [
                "standard-library module shadow_stdlib_module does not load from the standard library; remove the shadowing copy"])
            sys.modules.pop("shadow_stdlib_module", None)
        with patch.object(probe.importlib, "import_module", side_effect=OSError("libssl.so.3")):
            self.assertEqual(probe.stdlib_problems(directories, ("json",)), [
                "standard-library module json cannot load; rebuild or reinstall CPython with its system libraries"])

    def test_bitsandbytes_library_matches_the_torch_cuda_build(self):
        with tempfile.TemporaryDirectory() as directory:
            package = Path(directory)
            spec = SimpleNamespace(origin=str(package / "__init__.py"))
            with patch.object(probe.importlib.util, "find_spec", return_value=spec):
                self.assertEqual(probe.bitsandbytes_problem("13.0"), "bitsandbytes has no library for CUDA 13.0; install the bitsandbytes wheel from the lock")
                (package / "libbitsandbytes_cuda130.so").write_text("not a library")
                self.assertTrue(probe.bitsandbytes_problem("13.0").startswith("the bitsandbytes CUDA 13.0 library cannot load (OSError: "))
                with patch.object(probe.ctypes, "CDLL") as load:
                    self.assertIsNone(probe.bitsandbytes_problem("13.0"))
                    load.assert_called_once_with(str(package / "libbitsandbytes_cuda130.so"))
            with patch.object(probe.importlib.util, "find_spec", return_value=None):
                self.assertIsNone(probe.bitsandbytes_problem("13.0"))

    def test_probe_inserts_the_runtime_site_and_reports_torch_failures(self):
        with tempfile.TemporaryDirectory() as directory:
            def fail():
                raise RuntimeError("CUDA initialization failed\nmore")

            fake = SimpleNamespace(runtime_site=lambda: Path(directory), driver_cuda=lambda: None, torch_facts=fail)
            with patch.object(sys, "path", list(sys.path)):
                result = probe.probe(fake, True)
                self.assertEqual(sys.path[0], directory)
            self.assertEqual(result, {"stdlib": [], "facts": None, "torchError": "RuntimeError: CUDA initialization failed",
                                      "driverCuda": None, "bitsandbytes": None})
            fake.torch_facts = lambda: cuda_facts()
            with patch.object(sys, "path", list(sys.path)), patch.object(probe, "bitsandbytes_problem", return_value="missing") as library:
                self.assertEqual(probe.probe(fake, True)["bitsandbytes"], "missing")
                library.assert_called_once_with("13.0")
                self.assertIsNone(probe.probe(fake, False)["bitsandbytes"])
                fake.torch_facts = lambda: cuda_facts(cuda=None)
                self.assertIsNone(probe.probe(fake, True)["bitsandbytes"])
                library.assert_called_once()


if __name__ == "__main__":
    unittest.main()
