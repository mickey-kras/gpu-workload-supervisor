"""Guard the subset boundary, the exact deviation contract and the runtime report."""
import base64
import hashlib
import importlib.metadata
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import sysconfig
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch

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


def wheel(name, version="1.0", tag="py3-none-any", record="", files=()):
    texts = {"WHEEL": f"Wheel-Version: 1.0\nTag: {tag}\n" if tag else None, "RECORD": record}
    return SimpleNamespace(metadata={"Name": name}, version=version, files=[Path(file) for file in files],
                           read_text=texts.get)


def record_hash(data):
    return "sha256=" + base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()


def cuda_facts(**changes):
    facts = {
        "torch": "2.14.0", "cuda": "13.0", "hip": None, "available": True, "driverCuda": "13.0",
        "archList": ["sm_80", "sm_86", "sm_90", "sm_120"], "devices": [[8, 6]], "bitsandbytes": True,
    }
    facts.update(changes)
    return facts


def probe_result(**changes):
    result = {"stdlib": [], "facts": cuda_facts(), "torchError": None, "bitsandbytes": None}
    result.update(changes)
    return result


class FakeProcess:
    def __init__(self, codes):
        self.codes, self.pid, self.timeouts = list(codes), 4321, []

    def wait(self, timeout=None):
        self.timeouts.append(timeout)
        code = self.codes.pop(0)
        if code is None:
            raise subprocess.TimeoutExpired("probe", timeout)
        return code


def fake_popen(result=None, codes=(0,), stderr=b"", stdout=b"", raw=None):
    """Return a Popen replacement that writes the probe output and result file."""
    calls = []

    def popen(command, **options):
        calls.append((command, options))
        options["stdout"].write(stdout)
        options["stderr"].write(stderr)
        if raw is not None or result is not None:
            Path(command[-1]).write_bytes(raw if raw is not None else json.dumps(result).encode())
        return FakeProcess(codes)

    return popen, calls


FLAGS = {name: getattr(sys.flags, name) for name in dir(sys.flags) if isinstance(getattr(sys.flags, name), int) and not name.startswith("_")}
def alive(pid):
    # A zombie counts as gone: it runs no code and its parent reaps it.
    try:
        state = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[0]
    except (OSError, IndexError):
        return False
    return state != "Z"


ISOLATED = SimpleNamespace(**{**FLAGS, "isolated": 1, "no_site": 1})
RETRY = "reinstall the training runtime from the lock and run the check again"
SOURCE = (ROOT / "internal/llmtraining/worker.py").read_bytes()
DRIVER = "check the NVIDIA driver and the GPU, then run the check again"


class RuntimeTests(unittest.TestCase):
    def test_limits(self):
        self.assertEqual((checker.PROBE_TIMEOUT, checker.KILL_TIMEOUT, checker.PROBLEM_LIMIT, checker.PROBLEM_LENGTH),
                         (300, 10, 20, 300))

    def test_presets_come_from_the_asset_table_when_main_runs(self):
        presets = json.loads((ROOT / "training/assets.json").read_text())
        self.assertEqual(checker.load_presets(), tuple(presets))
        self.assertLessEqual(checker.QUANTIZED, set(presets))
        with tempfile.TemporaryDirectory() as directory:
            for text in ("{broken", "[]", "{}", '["smoke-lora"]'):
                (Path(directory) / "assets.json").write_text(text)
                with self.subTest(text=text), self.assertRaises(checker.InputError) as raised:
                    checker.load_presets(Path(directory) / "assets.json")
                self.assertEqual(raised.exception.remedy, "restore assets.json from the repository checkout")
            with self.assertRaises(checker.InputError):
                checker.load_presets(Path(directory) / "missing.json")
        for path, shown in ((checker.ASSETS, "training/assets.json"), (checker.LOCK, "training/axolotl/requirements.txt")):
            remedy = checker.InputError(path, OSError("gone")).remedy
            self.assertEqual(remedy, f"restore {shown} from the repository checkout")
            self.assertEqual(checker.remedy_for(checker.InputError(path, OSError("gone")), worker.SYNC), remedy)

    def main(self, arguments, **patches):
        with patch("sys.stdout", new_callable=io.StringIO) as output, patch.object(checker.sys, "flags", ISOLATED), \
                patch.object(checker.sys, "path", list(sys.path)):
            items = [patch.object(checker, name, value) for name, value in patches.items()]
            for item in items:
                item.start()
            try:
                code = checker.main(arguments)
            finally:
                for item in reversed(items):
                    item.stop()
        return code, output.getvalue()

    def test_broken_inputs_give_exit_2_with_the_stable_report(self):
        with tempfile.TemporaryDirectory() as directory:
            assets, broken = Path(directory) / "assets.json", Path(directory) / "worker.py"
            assets.write_text("{broken")
            broken.write_text("raise RuntimeError('broken worker')\n")
            for patches, target, remedy in (({"ASSETS": assets}, None, "restore assets.json from the repository checkout"),
                                            ({"WORKER": broken}, "cpu-smoke", checker.CHECKER_BUG)):
                with self.subTest(patches=patches):
                    code, output = self.main([], **patches)
                    result = json.loads(output)
                    self.assertEqual(code, 2)
                    self.assertEqual(set(result), STABLE_REPORT_KEYS | {"error"})
                    self.assertEqual((result["status"], result["target"], result["checks"]), ("rejected", target, []))
                    self.assertTrue(result["error"].startswith("check could not run ("))
                    self.assertTrue(result["error"].endswith(remedy), result["error"])

    def test_main_exit_codes_and_usage_errors(self):
        for status, code in (("accepted", 0), ("rejected", 1)):
            with self.subTest(status=status), patch.object(checker, "report", return_value={"status": status}) as report:
                uuid = "GPU-0123abcd-89ab-cdef-0123-456789ABCDEF"
                result, output = self.main(["--target", "representative-qlora", "--gpu-uuid", uuid])
                self.assertEqual((result, json.loads(output)), (code, {"status": status}))
                self.assertEqual(report.call_args.args[1:], ("representative-qlora", uuid))
        with patch.object(checker, "report", side_effect=RuntimeError(f"CUDA init in {checker.runtime_root()}/lib")):
            code, output = self.main([])
        self.assertEqual(code, 2)
        self.assertEqual(json.loads(output)["error"], f"check could not run (RuntimeError: CUDA init in <runtime>/lib); {checker.CHECKER_BUG}")
        for arguments in (["--target", "unknown"], ["--gpu-uuid", "MIG-0123abcd-89ab-cdef-0123-456789abcdef"],
                          ["--gpu-uuid", "GPU-0123abcd-89ab-cdef-0123-456789ABCDEF0"]):
            with self.subTest(arguments=arguments), patch("sys.stderr", new_callable=io.StringIO), self.assertRaises(SystemExit) as exit:
                self.main(arguments)
            self.assertEqual(exit.exception.code, 2)
        with patch.object(checker.sys, "flags", SimpleNamespace(**{**FLAGS, "isolated": 0, "no_site": 1})), \
                patch("sys.stderr", new_callable=io.StringIO) as error, \
                self.assertRaises(SystemExit) as exit:
            checker.main([])
        self.assertEqual(exit.exception.code, 2)
        self.assertIn("-I -S -B", error.getvalue())
        with patch.object(checker.sys, "flags", SimpleNamespace(**{**FLAGS, "isolated": 1, "no_site": 0})), \
                patch("sys.stderr", new_callable=io.StringIO), self.assertRaises(SystemExit) as exit:
            checker.main([])
        self.assertEqual(exit.exception.code, 2)

    def test_gpu_uuid_follows_the_go_rule(self):
        valid = "GPU-0123abcd-89ab-cdef-0123-456789ABCDEF"
        self.assertTrue(checker.GPU_UUID.fullmatch(valid))
        for value in (valid[:-1], valid + "0", "MIG" + valid[3:], "gpu" + valid[3:], valid.replace("-89ab", "_89ab"),
                      valid.replace("0123abcd", "0123abcg"), "GPU-0123abcd89ab-cdef-0123-456789ABCDEF0", "GPU-" + "0" * 36):
            with self.subTest(value=value):
                self.assertIsNone(checker.GPU_UUID.fullmatch(value))

    def test_runner_layout_needs_a_regular_self_contained_root(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "runtime"
            site = root / "lib/python3.12/site-packages"
            (root / "bin").mkdir(parents=True)
            site.mkdir(parents=True)
            (root / "bin/python3").write_text("interpreter")
            self.assertEqual(checker.layout_problems(root, site.parent, site), [])
            (root / "pyvenv.cfg").write_text("home = /usr/bin\n")
            (root / "bin/python3").unlink()
            (root / "bin/python3").symlink_to(root / "bin/python3.12")
            site.rmdir()
            self.assertEqual(checker.layout_problems(root, Path(directory) / "base/lib", site), [
                "the runtime is a virtual environment; the runner needs a self-contained CPython root",
                "bin/python3 is not a regular file; copy the interpreter into the runtime root",
                "the runtime site-packages is not a regular directory in the runtime root",
                "the standard library is outside the runtime root; install it into the same root",
            ])
        self.assertEqual(checker.runtime_root(), Path(sys.executable).parent.parent)

    def test_site_packages_must_not_shadow_or_customize_the_standard_library(self):
        with tempfile.TemporaryDirectory() as directory:
            stdlib, site = Path(directory) / "stdlib", Path(directory) / "site"
            for name in ("_sysconfigdata__linux_x86_64-linux-gnu.py", "LICENSE.txt", "site-packages/", "lib-dynload/",
                         "lib-dynload/_private_native.cpython-312-x86_64-linux-gnu.so", "test/", "test/__init__.py",
                         "extra_public.py"):
                path = stdlib / name.rstrip("/")
                path.parent.mkdir(parents=True, exist_ok=True)
                path.mkdir() if name.endswith("/") else path.write_text("")
            forbidden = checker.stdlib_names([stdlib, stdlib / "lib-dynload", stdlib / "missing"])
            self.assertLessEqual({"_sysconfigdata__linux_x86_64-linux-gnu", "_private_native", "json"}, forbidden)
            # Some builds ship the public test package; torchao installs a top-level test package.
            self.assertFalse({"LICENSE", "LICENSE.txt", "site-packages", "test", "extra_public"} & forbidden)
            site.mkdir()
            owned = b"import os\n"  # The content does not matter: .pth files never run with -S.
            for name in ("json.py", "json.txt", "sitecustomize.py", "usercustomize.pyc", "_ssl.cpython-312-x86_64-linux-gnu.so",
                         "_sysconfigdata__linux_x86_64-linux-gnu.py", "_private_native.py", "torch/", "email/", "test/",
                         "torch-2.14.0.dist-info/", "owned.pth", "stray.pth", "README.txt"):
                if name.endswith("/"):
                    (site / name).mkdir()
                else:
                    (site / name).write_bytes(owned if name.endswith(".pth") else b"")
            shadows, startup = checker.site_problems(site, {"owned.pth"}, forbidden)
            self.assertEqual(shadows, [
                "site-packages has _private_native.py, which shadows the standard-library module _private_native; remove it",
                "site-packages has _ssl.cpython-312-x86_64-linux-gnu.so, which shadows the standard-library module _ssl; remove it",
                "site-packages has _sysconfigdata__linux_x86_64-linux-gnu.py, which shadows the standard-library module "
                "_sysconfigdata__linux_x86_64-linux-gnu; remove it",
                "site-packages has email, which shadows the standard-library module email; remove it",
                "site-packages has json.py, which shadows the standard-library module json; remove it",
                "site-packages has sitecustomize.py; remove the startup customization",
                "site-packages has usercustomize.pyc; remove the startup customization",
            ])
            self.assertEqual(startup, [
                "site-packages has the startup file stray.pth, which no locked wheel installs; remove it",
            ])
            self.assertEqual(checker.site_problems(site / "missing", {}, forbidden), ([], []))

    def test_only_startup_files_recorded_by_locked_wheels_are_owned(self):
        distributions = [
            wheel("coloredlogs", files=["coloredlogs.pth", "coloredlogs/__init__.py"]),
            wheel("unlocked", files=["unlocked.pth"]),
            wheel("setuptools", files=["setuptools/nested.pth", "distutils-precedence.pth"]),
        ]
        owned = checker.owned_startup_files(distributions, {"coloredlogs": Version("1"), "setuptools": Version("1")})
        self.assertEqual(owned, {"coloredlogs.pth", "distutils-precedence.pth"})

    def test_locked_versions_read_hash_locked_requirements(self):
        text = "Torch==2.14.0 \\\n    --hash=sha256:abc\nnvidia_cublas==13.1.1.3 \\\n    --hash=sha256:def\n# comment==1\nmarker==1.0 ; sys_platform != 'darwin'\n"
        self.assertEqual(checker.locked_versions(text), {"torch": Version("2.14.0"), "nvidia-cublas": Version("13.1.1.3"), "marker": Version("1.0")})
        locked = checker.locked_versions(checker.LOCK.read_text(encoding="utf-8"))
        self.assertEqual(locked["torch"], Version("2.14.0"))
        self.assertNotIn("axolotl", locked)

    def test_installed_wheels_match_the_lock_and_this_interpreter(self):
        supported = {Tag("py3", "none", "any"), Tag("cp312", "cp312", "manylinux_2_28_x86_64")}
        locked = {"torch": Version("2.14.0"), "peft": Version("0.21.2")}
        exact = [wheel("torch", "2.14.0", "cp312-cp312-manylinux_2_28_x86_64"), wheel("PEFT", "0.21.2"), wheel("axolotl", "0.21.0", None)]
        self.assertEqual(checker.lock_problems(locked, exact, supported, "SYNC"), [])
        changed = [wheel("torch", "2.13.0", "cp311-cp311-manylinux_2_28_x86_64"), wheel("unlocked_extra"), wheel("axolotl")]
        self.assertEqual(checker.lock_problems(locked, changed, supported, "SYNC"), [
            "peft==0.21.2 from the lock is missing; SYNC",
            "unlocked-extra is not in the lock; SYNC",
            "torch 2.13.0 differs from the locked 2.14.0; SYNC",
            "torch is not a wheel for this interpreter and platform; SYNC",
        ])
        self.assertEqual(checker.lock_problems({"peft": Version("0.21.2")}, [wheel("peft", "0.21.2", None)], supported, "SYNC"),
                         ["peft is not a wheel for this interpreter and platform; SYNC"])
        multi = SimpleNamespace(metadata={"Name": "torch"}, version="2.14.0", files=[], read_text={
            "WHEEL": "Wheel-Version: 1.0\nTag: cp311-cp311-manylinux_2_28_x86_64\nTag: cp312-cp312-manylinux_2_28_x86_64\n"}.get)
        for distributions in ([wheel("torch", "2.14.0", "py2.py3-none-any"), wheel("peft", "0.21.2")], [multi, wheel("peft", "0.21.2")]):
            with self.subTest(distributions=distributions):
                self.assertEqual(checker.lock_problems(locked, distributions, supported, "SYNC"), [])

    def test_dependency_problems_carry_one_remedy_each(self):
        versions = {"python-dotenv": "1.2.2", "fsspec": "2026.6.0", "pillow": "13.0"}

        def raising(error):
            def verify():
                if error is not None:
                    raise error
            return SimpleNamespace(SYNC=worker.SYNC, SOURCE=worker.SOURCE, verify_environment=verify)

        with patch.object(checker, "deviations", return_value=set()):
            problems = checker.dependency_problems(raising(None))
            self.assertEqual(len(problems), 1)
            self.assertTrue(problems[0].startswith("dependency deviations changed: missing="))
            self.assertTrue(problems[0].endswith(worker.SYNC))
        with patch.object(checker, "deviations", side_effect=ValueError("duplicate distribution: torch")):
            self.assertEqual(checker.dependency_problems(raising(None)), [f"duplicate distribution: torch; {worker.SYNC}"])
        with patch.object(checker, "deviations", return_value=checker.EXPECTED), patch.object(checker.metadata, "version", side_effect=versions.get):
            self.assertEqual(checker.dependency_problems(raising(None)), [])
            for error, expected in (
                (ValueError("runtime source is not pinned; " + worker.SOURCE), "runtime source is not pinned; " + worker.SOURCE),
                (importlib.metadata.PackageNotFoundError("axolotl"), "axolotl is not installed; " + worker.SOURCE),
                (importlib.metadata.PackageNotFoundError("peft"), "peft is not installed; " + worker.SYNC),
            ):
                with self.subTest(error=error):
                    self.assertEqual(checker.dependency_problems(raising(error)), [expected])
            versions["pillow"] = "12.0"
            self.assertEqual(checker.dependency_problems(raising(None)), [f"security floor not met: pillow; {worker.SYNC}"])
        with patch.object(checker, "deviations", return_value=checker.EXPECTED), \
                patch.object(checker.metadata, "version", side_effect=importlib.metadata.PackageNotFoundError("fsspec")):
            self.assertEqual(checker.dependency_problems(raising(None)), [f"fsspec is not installed; {worker.SYNC}"])

    def test_worker_messages_carry_their_remedy(self):
        self.assertIn(worker.SOURCE_REVISION, worker.SOURCE)
        with patch.object(worker.importlib.metadata, "distributions", return_value=[distribution("gradio")]):
            with self.assertRaisesRegex(ValueError, "unapproved optional runtime integration: gradio; " + worker.SYNC):
                worker.verify_subset()

    def test_probe_environment_has_exactly_the_runner_values(self):
        work = Path("/work")
        with patch.dict(os.environ, {"LD_LIBRARY_PATH": "/injected", "LD_PRELOAD": "/injected.so", "BNB_CUDA_VERSION": "128"}):
            environment = checker.probe_environment(work, None)
        self.assertEqual(environment, {
            "PATH": f"{Path(sys.executable).parent}:/usr/bin:/bin", "HOME": "/work/home", "XDG_CONFIG_HOME": "/work/home",
            "TMPDIR": "/work", "HF_HOME": "/work/cache", "HF_HUB_OFFLINE": "1", "HF_DATASETS_OFFLINE": "1",
            "TRANSFORMERS_OFFLINE": "1", "HF_HUB_DISABLE_TELEMETRY": "1", "AXOLOTL_DO_NOT_TRACK": "1", "DO_NOT_TRACK": "1",
            "WANDB_MODE": "disabled", "TOKENIZERS_PARALLELISM": "false", "LANG": "C.UTF-8",
        })
        self.assertEqual(checker.probe_environment(work, "GPU-1")["CUDA_VISIBLE_DEVICES"], "GPU-1")
        runner = (ROOT / "internal/llmtraining/run.go").read_text()
        for name in [*environment, "CUDA_VISIBLE_DEVICES"]:
            self.assertIn(f'"{name}=', runner)

    def test_run_probe_uses_a_runner_like_child_and_the_result_file(self):
        popen, calls = fake_popen(probe_result(), stdout=b"third-party noise\n")
        self.assertEqual(checker.run_probe(True, "GPU-1", SOURCE, popen), (probe_result(), None))
        command, options = calls[0]
        work = Path(options["env"]["TMPDIR"])
        self.assertEqual(command, [sys.executable, "-I", "-S", "-B", str(checker.PROBE), str(work / "worker.py"), "quantized",
                                   str(work / "result.json")])
        self.assertEqual((options["stdin"], options["start_new_session"], options["cwd"], options["env"]["CUDA_VISIBLE_DEVICES"]),
                         (subprocess.DEVNULL, True, work, "GPU-1"))
        self.assertFalse(work.exists())
        popen, calls = fake_popen(probe_result())
        self.assertEqual(checker.run_probe(False, None, SOURCE, popen), (probe_result(), None))
        self.assertEqual(calls[0][0][-2], "plain")

    def test_probe_failures_map_to_actionable_problems(self):
        noise = b"x" * (checker.OUTPUT_LIMIT * 2) + b"\nImportError: libtorch_cuda.so: cannot open\n\n"
        for options, expected in (
            ({"codes": (1,), "stderr": noise}, f"the runtime probe failed (ImportError: libtorch_cuda.so: cannot open); {RETRY}"),
            ({"codes": (1,), "stdout": b"only stdout\n"}, f"the runtime probe failed (only stdout); {RETRY}"),
            ({"codes": (3,)}, f"the runtime probe failed (exit status 3); {RETRY}"),
            ({"codes": (-9,)}, "the runtime probe was killed by signal SIGKILL (often out of memory); "
                               "free memory or raise the memory limit and run the check again"),
            ({"codes": (-11,)}, "the runtime probe was killed by signal SIGSEGV; check the system log and run the check again"),
            ({}, f"the runtime probe gave no valid result; {RETRY}"),
            ({"raw": b"{broken"}, f"the runtime probe gave no valid result; {RETRY}"),
            ({"raw": b" " * (checker.OUTPUT_LIMIT + 1)}, f"the runtime probe gave no valid result; {RETRY}"),
        ):
            with self.subTest(options=options):
                self.assertEqual(checker.run_probe(False, None, SOURCE, fake_popen(**options)[0]), (None, expected))
        for changed in (
            [1], {}, {**probe_result(), "extra": 1}, {key: value for key, value in probe_result().items() if key != "stdlib"},
            probe_result(stdlib=[1]), probe_result(torchError=1), probe_result(bitsandbytes=[]),
            probe_result(facts=cuda_facts(torch=None)), probe_result(facts=cuda_facts(torch="x" * 65)),
            probe_result(facts=cuda_facts(available="yes")), probe_result(facts=cuda_facts(bitsandbytes=None)),
            probe_result(facts=cuda_facts(archList="sm_86")), probe_result(facts=cuda_facts(archList=["sm_86"] * 65)),
            probe_result(facts=cuda_facts(devices=[[8]])), probe_result(facts=cuda_facts(devices=[[8, 6.0]])),
            probe_result(facts=cuda_facts(devices=[[8, True]])), probe_result(facts=cuda_facts(driverCuda=13)),
            probe_result(facts=cuda_facts(cuda=13)), probe_result(facts=cuda_facts(archList=[1])),
            probe_result(facts=cuda_facts(archList=[None])), probe_result(facts=cuda_facts(devices=[[8, 6]] * 65)),
            probe_result(facts={key: value for key, value in cuda_facts().items() if key != "hip"} | {"hip": 1}),
        ):
            with self.subTest(changed=changed):
                self.assertEqual(checker.run_probe(False, None, SOURCE, fake_popen(raw=json.dumps(changed).encode())[0]),
                                 (None, f"the runtime probe gave no valid result; {RETRY}"))

    def test_schema_accepts_the_limits(self):
        result = probe_result(facts=cuda_facts(torch="x" * 64, archList=["sm_86"] * 64, devices=[[8, 6]] * 64))
        self.assertEqual(checker.decode_probe(json.dumps(result).encode()), result)

    def test_stderr_wins_over_stdout_and_bad_bytes_are_replaced(self):
        for options, expected in (
            ({"stdout": b"noise\n", "stderr": b"ImportError: x\n"}, "ImportError: x"),
            ({"stderr": b"\xff bad\n"}, "\ufffd bad"),
        ):
            with self.subTest(expected=expected):
                self.assertEqual(checker.run_probe(False, None, SOURCE, fake_popen(codes=(1,), **options)[0], lambda *args: None),
                                 (None, f"the runtime probe failed ({expected}); {RETRY}"))

    def test_timeouts_kill_the_process_group_without_blocking(self):
        for codes, expected in (
            ((None, -9), f"the runtime probe did not finish in 300 seconds; {DRIVER}"),
            ((None, None), f"the runtime probe hung in the driver and did not stop; {DRIVER}"),
        ):
            killed = []
            with self.subTest(codes=codes):
                result = checker.run_probe(False, None, SOURCE, fake_popen(codes=codes)[0], lambda pid, number: killed.append((pid, number)))
                self.assertEqual(result, (None, expected))
                self.assertEqual(killed, [(4321, signal.SIGKILL)])

    def test_the_process_group_is_always_stopped_and_a_gone_group_is_not_an_error(self):
        killed = []
        self.assertEqual(checker.run_probe(False, None, SOURCE, fake_popen(probe_result())[0], lambda pid, number: killed.append(number)),
                         (probe_result(), None))
        self.assertEqual(killed, [signal.SIGKILL])

        def gone(pid, number):
            raise ProcessLookupError(pid)

        process = FakeProcess((0,))
        self.assertEqual((checker.wait_for_probe(process, gone), process.timeouts), ((0, None), [300]))
        process = FakeProcess((None, -9))
        self.assertEqual((checker.wait_for_probe(process, gone), process.timeouts),
                         ((None, f"the runtime probe did not finish in 300 seconds; {DRIVER}"), [300, 10]))
        process = FakeProcess((None, None))
        self.assertEqual((checker.wait_for_probe(process, gone), process.timeouts),
                         ((None, f"the runtime probe hung in the driver and did not stop; {DRIVER}"), [300, 10]))

    def test_a_timeout_stops_the_whole_probe_process_group(self):
        with tempfile.TemporaryDirectory() as directory:
            record = Path(directory) / "grandchild"
            script = Path(directory) / "probe.py"
            script.write_text("import subprocess, time\n"
                              "child = subprocess.Popen(['sleep', '60'])\n"
                              f"open({str(record)!r}, 'w').write(str(child.pid))\n"
                              "time.sleep(60)\n")
            with patch.object(checker, "PROBE", script), patch.object(checker, "PROBE_TIMEOUT", 1):
                result = checker.run_probe(False, None, SOURCE)
            self.assertEqual(result, (None, f"the runtime probe did not finish in 1 seconds; {DRIVER}"))
            pid = int(record.read_text())
            for _ in range(50):
                if not alive(pid):
                    break
                time.sleep(0.1)
            self.assertFalse(alive(pid))

    def test_fifo_outputs_never_block_the_checker(self):
        def fifo_result(command, **options):
            os.mkfifo(command[-1])
            return FakeProcess((0,))

        def fifo_stderr(command, **options):
            options["stderr"].write(b"ImportError: before the swap\n")
            options["stderr"].flush()
            path = Path(options["cwd"]) / "stderr"
            path.unlink()
            os.mkfifo(path)
            os.mkfifo(command[-1])
            return FakeProcess((1,))

        for popen, expected in (
            (fifo_result, f"the runtime probe gave no valid result; {RETRY}"),
            (fifo_stderr, f"the runtime probe failed (ImportError: before the swap); {RETRY}"),
        ):
            with self.subTest(expected=expected):
                outcome = []
                thread = threading.Thread(target=lambda: outcome.append(checker.run_probe(False, None, SOURCE, popen, lambda *args: None)),
                                          daemon=True)
                thread.start()
                thread.join(10)
                self.assertFalse(thread.is_alive(), "the checker blocked on a FIFO")
                self.assertEqual(outcome, [(None, expected)])

    def test_result_file_is_bounded_and_not_followed_through_a_link(self):
        with tempfile.TemporaryDirectory() as directory:
            target, link = Path(directory) / "target.json", Path(directory) / "result.json"
            target.write_text(json.dumps(probe_result()))
            link.symlink_to(target)
            self.assertEqual(checker.read_result(link), b"")
            self.assertEqual(checker.read_result(Path(directory) / "missing.json"), b"")
            self.assertEqual(checker.decode_probe(checker.read_result(target)), probe_result())
            target.write_bytes(b" " * (checker.OUTPUT_LIMIT + 10))
            self.assertEqual(len(checker.read_result(target)), checker.OUTPUT_LIMIT + 1)
        self.assertIsNone(checker.decode_probe(b" " * (checker.OUTPUT_LIMIT + 1)))
        valid = json.dumps(probe_result()).encode()
        self.assertIsNone(checker.decode_probe(valid + b" " * checker.OUTPUT_LIMIT))
        self.assertEqual(checker.decode_probe(valid + b" " * (checker.OUTPUT_LIMIT - len(valid))), probe_result())
        self.assertIsNone(checker.decode_probe(b"{broken"))
        self.assertIsNone(checker.decode_probe(b"[]"))

        def linked(command, **options):
            Path(command[-1]).symlink_to("/etc/hostname")
            return FakeProcess((0,))

        self.assertEqual(checker.run_probe(False, None, SOURCE, linked, lambda *args: None), (None, f"the runtime probe gave no valid result; {RETRY}"))

    def test_failing_child_process_maps_to_an_actionable_problem(self):
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / "probe.py"
            script.write_text("import sys\nsys.stderr.write('OSError: libcudnn.so.9: cannot open shared object file\\n')\nsys.exit(3)\n")
            with patch.object(checker, "PROBE", script):
                probe, failure = checker.run_probe(False, None, SOURCE)
        self.assertIsNone(probe)
        self.assertEqual(failure, f"the runtime probe failed (OSError: libcudnn.so.9: cannot open shared object file); {RETRY}")
        problems = checker.probe_checks(worker, None, failure, "cpu-smoke", None)
        self.assertEqual(problems["native"], [failure])
        self.assertEqual(problems["accelerator"], ["the runtime probe did not run; fix the native-libraries problem first"])

    def real_probe(self, mode_quantized, facts):
        with tempfile.TemporaryDirectory() as directory:
            fake = Path(directory) / "worker.py"
            fake.write_text(
                "import json, os, sys\nfrom pathlib import Path\n"
                "def runtime_site():\n"
                "    site = Path(os.environ['TMPDIR']) / 'site'\n"
                "    (site / 'bitsandbytes').mkdir(parents=True, exist_ok=True)\n"
                "    (site / 'bitsandbytes' / '__init__.py').write_text('raise SystemExit(\"bitsandbytes imported\")')\n"
                "    return site\n"
                f"FACTS = {facts!r}\n"
                "def torch_facts():\n"
                "    if FACTS is None:\n"
                "        flags = (sys.flags.isolated, sys.flags.no_site, sys.flags.dont_write_bytecode)\n"
                "        raise ImportError('no torch ' + repr(flags) + ' ' + str('LD_PRELOAD' in os.environ))\n"
                "    return json.loads(FACTS)\n")
            with patch.object(checker, "WORKER", fake), patch.dict(os.environ, {"LD_PRELOAD": "/injected.so"}):
                return checker.run_probe(mode_quantized, "GPU-1", fake.read_bytes())

    def test_real_probe_child_runs_isolated_with_a_fake_worker(self):
        self.assertEqual(self.real_probe(True, None), (
            {"stdlib": [], "facts": None, "torchError": "ImportError: no torch (1, 1, 1) False", "bitsandbytes": None}, None))
        facts = cuda_facts()
        self.assertEqual(self.real_probe(False, json.dumps(facts)), ({"stdlib": [], "facts": facts, "torchError": None, "bitsandbytes": None}, None))
        probe, failure = self.real_probe(True, json.dumps(facts))
        self.assertEqual((probe["bitsandbytes"], failure), ("bitsandbytes has no library for CUDA 13.0; install the bitsandbytes wheel from the lock", None))

    def test_probe_results_map_to_check_problems(self):
        needs = "this check needs Torch; fix the native-libraries problem first"
        cases = (
            (probe_result(), "smoke-lora", "GPU-1", {"stdlib": [], "native": [], "driver": [], "accelerator": []}),
            (probe_result(torchError="OSError: libcudnn.so.9", facts=None), "smoke-lora", None, {
                "stdlib": [], "native": ["Torch cannot load (OSError: libcudnn.so.9); reinstall torch and its CUDA wheels from the lock"],
                "driver": [needs], "accelerator": ["no trainer GPU is selected; pass --gpu-uuid with the trainer GPU UUID", needs]}),
            (probe_result(facts=cuda_facts(driverCuda=None, available=False)), "cpu-smoke", None, {
                "stdlib": [], "native": [], "driver": ["the NVIDIA driver library libcuda.so.1 does not load; install the NVIDIA driver"],
                "accelerator": ["Torch CUDA 13.0 cannot use a GPU; install an NVIDIA driver for CUDA 13.0 or newer and expose one GPU"]}),
            (probe_result(bitsandbytes="bitsandbytes has no library for CUDA 13.0", stdlib=["broken"]), "representative-qlora", "GPU-1", {
                "stdlib": ["broken"], "native": ["bitsandbytes has no library for CUDA 13.0"], "driver": [], "accelerator": []}),
            (probe_result(facts=cuda_facts(bitsandbytes=False)), "representative-qlora", "GPU-1", {
                "stdlib": [], "native": [], "driver": [], "accelerator": ["QLoRA needs bitsandbytes; install bitsandbytes from the training lock"]}),
            (probe_result(facts=cuda_facts(bitsandbytes=False)), "smoke-lora", "GPU-1", {"stdlib": [], "native": [], "driver": [], "accelerator": []}),
        )
        for probe, target, gpu, expected in cases:
            with self.subTest(target=target, probe=probe):
                self.assertEqual(checker.probe_checks(worker, probe, None, target, gpu), expected)

    def test_check_status_bounds_and_masks_problems(self):
        problems = [str(index) for index in range(22)]
        self.assertEqual(checker.check("accelerator", [], "smoke-lora", cpu_smoke_warning=True)["status"], "pass")
        self.assertEqual(checker.check("accelerator", ["no GPU"], "cpu-smoke", cpu_smoke_warning=True)["status"], "warn")
        for target in ("smoke-lora", "representative-qlora"):
            self.assertEqual(checker.check("runtime-layout", ["no GPU"], target, cpu_smoke_warning=True)["status"], "fail")
        self.assertEqual(checker.check("python", problems, "cpu-smoke"), {"name": "python", "status": "fail", "problems": problems[:20], "omitted": 2})
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"HOME": "/home/operator"}):
            text = (f"x {checker.runtime_root()}/lib/a.so {sys.base_prefix}/b /home/operator/c {directory}/d {os.getcwd()}/e "
                    "/opt/secret/f.so in lib/g.so /home/operatorX/z " + "y" * 400)
            masked = checker.check("python", [text], "cpu-smoke")["problems"][0]
        self.assertTrue(masked.startswith("x <runtime>/lib/a.so "), masked)
        self.assertNotIn("<home>X", masked)
        for token in ("/b <home>/c <tmp>", " <cwd>/e <path> in lib/g.so <path> y"):
            self.assertIn(token, masked)
        for literal in (directory, sys.base_prefix + "/b", "/home/operator", "/opt/secret"):
            self.assertNotIn(literal, masked)
        self.assertEqual(len(masked), 300)
        with patch.object(checker, "runtime_root", return_value=Path("/runtime-root")):
            self.assertIn("<python>/b", checker.mask(f"{sys.base_prefix}/b"))
        with patch.dict(os.environ, {"HOME": "/"}):
            self.assertEqual(checker.mask("see /opt/x"), "see <path>")
            self.assertEqual(checker.mask("/opt/x"), "<path>")
        with patch.dict(os.environ, clear=True):
            self.assertEqual(checker.mask("/opt/x"), "<path>")
        with patch.object(checker, "runtime_root", return_value=Path("/runtime-root")):
            self.assertIn("<python>/b", checker.mask(f"x {sys.base_prefix}/b y"))
        with patch.dict(os.environ, {"HOME": "/home/operator"}):
            for text, expected in (("at /home/operator", "at <home>"), ("(/home/operator)", "(<home>)"),
                                   ("/home/operator; next", "<home>; next")):
                self.assertEqual(checker.mask(text), expected)

    def test_a_deleted_working_directory_still_gives_a_json_report(self):
        previous = os.getcwd()
        directory = tempfile.mkdtemp()
        os.chdir(directory)
        os.rmdir(directory)
        try:
            self.assertEqual(checker.mask("at /opt/x"), "at <path>")
            with patch.object(checker, "report", side_effect=RuntimeError("broken")):
                code, output = self.main([])
        finally:
            os.chdir(previous)
        self.assertEqual((code, json.loads(output)["status"]), (2, "rejected"))

    def test_a_broken_check_is_a_failure_entry_but_interrupts_propagate(self):
        def broken():
            raise KeyError("facts")

        self.assertEqual(checker.guarded("accelerator", broken, "cpu-smoke", worker.SYNC, cpu_smoke_warning=True), {
            "name": "accelerator", "status": "fail", "problems": [f"check could not run (KeyError: 'facts'); {checker.CHECKER_BUG}"], "omitted": 0})

        def interrupted():
            raise KeyboardInterrupt

        with self.assertRaises(KeyboardInterrupt):
            checker.guarded("python", interrupted, "cpu-smoke", worker.SYNC)

        def missing():
            raise ImportError("No module named 'packaging'")

        self.assertEqual(checker.guarded("startup-files", missing, "cpu-smoke", worker.SYNC, cpu_smoke_warning=True)["status"], "fail")
        self.assertEqual(checker.remedy_for(OSError("x"), worker.SYNC), worker.SYNC)
        self.assertEqual(checker.remedy_for(OSError("x")), checker.CHECKER_BUG)
        self.assertEqual(checker.remedy_for(KeyError("x"), worker.SYNC), checker.CHECKER_BUG)

    def report(self, target, probe, gpu_uuid="GPU-1", clean=True, popen_override=None, **patches):
        popen, calls = fake_popen(probe)
        popen = popen_override or popen
        items = [patch.object(checker.metadata, "distributions", return_value=[])]
        if clean:
            items += [patch.object(checker, "lock_problems", return_value=[]), patch.object(checker, "dependency_problems", return_value=[]),
                      patch.object(checker, "site_problems", return_value=([], []))]
        items += [patch.object(checker, name, value) for name, value in patches.items()]
        for item in items:
            item.start()
        try:
            return checker.report(worker, target, gpu_uuid, popen, worker_source=SOURCE), calls
        finally:
            for item in reversed(items):
                item.stop()

    def test_report_passes_the_quantized_mode_to_the_probe(self):
        for target, mode in (("cpu-smoke", "plain"), ("smoke-lora", "plain"), ("representative-qlora", "quantized")):
            with self.subTest(target=target):
                _, calls = self.report(target, probe_result())
                self.assertEqual(calls[0][0][-2], mode)

    def test_report_keeps_stable_keys_and_accepts_a_clean_cpu_smoke(self):
        cpu = probe_result(facts=cuda_facts(available=False, archList=[], devices=[], driverCuda=None))
        result, _ = self.report("cpu-smoke", cpu, gpu_uuid=None)
        self.assertEqual(set(result), STABLE_REPORT_KEYS | {"error"})
        self.assertEqual((result["version"], result["status"], result["accelerator"], result["lockTarget"], result["error"]),
                         (1, "accepted", "cpu", checker.LOCK_TARGET, None))
        self.assertEqual(result["workerSHA256"], hashlib.sha256(checker.WORKER.read_bytes()).hexdigest())
        self.assertEqual(result["lockSHA256"], hashlib.sha256(checker.LOCK.read_bytes()).hexdigest())
        statuses = {item["name"]: item["status"] for item in result["checks"]}
        self.assertEqual((statuses["driver-libraries"], statuses["accelerator"]), ("warn", "warn"))
        self.assertTrue(all(status in {"pass", "warn"} for status in statuses.values()))

    def test_every_check_has_its_pinned_cpu_smoke_severity(self):
        with patch.object(worker, "python_problem", return_value="p"):
            result, _ = self.report("cpu-smoke", probe_result(stdlib=["s"], torchError="e", facts=None), clean=False,
                                    layout_problems=lambda *args: ["l"], site_problems=lambda *args: (["h"], ["t"]),
                                    dependency_problems=lambda *args: ["d"], lock_problems=lambda *args: ["v"])
        self.assertEqual({item["name"]: item["status"] for item in result["checks"]}, {
            "python": "fail", "runtime-layout": "warn", "standard-library": "fail", "startup-files": "warn",
            "dependencies": "fail", "locked-versions": "fail", "native-libraries": "fail", "driver-libraries": "warn",
            "accelerator": "warn"})
        self.assertEqual(result["accelerator"], None)

    def test_report_rejects_presets_with_preset_only_problems(self):
        result, _ = self.report("smoke-lora", probe_result(facts=cuda_facts(driverCuda=None)))
        self.assertEqual(result["status"], "rejected")
        self.assertEqual(next(item for item in result["checks"] if item["name"] == "driver-libraries")["status"], "fail")
        result, _ = self.report("representative-qlora", probe_result(facts=cuda_facts(bitsandbytes=False)))
        accelerator = next(item for item in result["checks"] if item["name"] == "accelerator")
        self.assertEqual((result["status"], result["accelerator"], result["driverCuda"], accelerator["status"]), ("rejected", "cuda", "13.0", "fail"))
        self.assertEqual(accelerator["problems"], ["QLoRA needs bitsandbytes; install bitsandbytes from the training lock"])

    def test_report_computes_the_shared_results_once(self):
        with patch.object(checker, "probe_checks", wraps=checker.probe_checks) as mapped, \
                patch.object(checker, "stdlib_names", wraps=checker.stdlib_names) as names:
            sites = []

            def site(*args):
                sites.append(args)
                return [], []

            self.report("cpu-smoke", probe_result(), clean=True, site_problems=site)
        # The site-packages scan runs once before and once after the probe.
        self.assertEqual((mapped.call_count, names.call_count, len(sites)), (1, 1, 2))

    def test_a_site_packages_change_during_the_probe_fails_the_standard_library(self):
        with tempfile.TemporaryDirectory() as directory:
            site = Path(directory)

            def planting(command, **options):
                (site / "json.py").write_text("")
                Path(command[-1]).write_text(json.dumps(probe_result()))
                return FakeProcess((0,))

            def hiding(command, **options):
                (site / "json.py").unlink()
                Path(command[-1]).write_text(json.dumps(probe_result()))
                return FakeProcess((0,))

            items = [patch.object(worker, "runtime_site", return_value=site), patch.object(checker.metadata, "distributions", return_value=[]),
                     patch.object(checker, "lock_problems", return_value=[]), patch.object(checker, "dependency_problems", return_value=[]),
                     patch.object(checker.os, "killpg")]
            for item in items:
                item.start()
            try:
                planted = checker.report(worker, "cpu-smoke", None, planting, worker_source=SOURCE)
                hidden = checker.report(worker, "cpu-smoke", None, hiding, worker_source=SOURCE)
            finally:
                for item in reversed(items):
                    item.stop()
        changed = f"site-packages changed while the runtime probe ran; {worker.SYNC}"
        for result in (planted, hidden):
            standard = next(item for item in result["checks"] if item["name"] == "standard-library")
            self.assertEqual(standard["status"], "fail")
            self.assertIn(changed, standard["problems"])

    def test_a_failed_scan_fails_both_site_checks(self):
        for side_effect in ([PermissionError("site-packages"), ([], [])], [([], []), PermissionError("site-packages")]):
            with self.subTest(side_effect=side_effect):
                result, _ = self.report("cpu-smoke", probe_result(), clean=False, dependency_problems=lambda *args: [],
                                        lock_problems=lambda *args: [], site_problems=MagicMock(side_effect=side_effect))
                statuses = {item["name"]: item for item in result["checks"]}
                for name in ("standard-library", "startup-files"):
                    self.assertEqual(statuses[name]["status"], "fail")
                    self.assertEqual(statuses[name]["problems"],
                                     [f"check could not run (PermissionError: site-packages); {worker.SYNC}"])

    def test_both_failed_scans_report_one_error(self):
        result, _ = self.report("cpu-smoke", probe_result(), clean=False, dependency_problems=lambda *args: [],
                                lock_problems=lambda *args: [],
                                site_problems=MagicMock(side_effect=[PermissionError("first"), PermissionError("second")]))
        statuses = {item["name"]: item for item in result["checks"]}
        for name in ("standard-library", "startup-files"):
            self.assertEqual(statuses[name]["status"], "fail")
            self.assertEqual(statuses[name]["problems"], [f"check could not run (PermissionError: first); {worker.SYNC}"])

    def test_probe_stdlib_problems_reach_the_standard_library_check(self):
        broken = "standard-library module _ssl cannot load; rebuild or reinstall CPython with its system libraries"
        result, _ = self.report("smoke-lora", probe_result(stdlib=[broken]))
        standard = next(item for item in result["checks"] if item["name"] == "standard-library")
        self.assertEqual((standard["status"], standard["problems"]), ("fail", [broken]))
        shadow = "site-packages has json.py, which shadows the standard-library module json; remove it"
        result, _ = self.report("smoke-lora", probe_result(stdlib=[broken]), clean=False, dependency_problems=lambda *args: [],
                                lock_problems=lambda *args: [], site_problems=lambda *args: ([shadow], []))
        standard = next(item for item in result["checks"] if item["name"] == "standard-library")
        self.assertEqual((standard["status"], standard["problems"]), ("fail", [broken, shadow]))

    def test_report_gives_lock_problems_the_tags_of_this_interpreter(self):
        from packaging.tags import sys_tags

        with patch.object(checker, "lock_problems", wraps=checker.lock_problems) as wrapped:
            self.report("cpu-smoke", probe_result(), clean=False, dependency_problems=lambda *args: [],
                        site_problems=lambda *args: ([], []))
        supported = wrapped.call_args.args[2]
        self.assertTrue(supported)
        self.assertIn(next(iter(sys_tags())), supported)

    def test_load_packaging_imports_every_submodule(self):
        names = {"packaging.requirements", "packaging.tags", "packaging.utils", "packaging.version"}
        with patch.dict(sys.modules):
            for name in [name for name in sys.modules if name == "packaging" or name.startswith("packaging.")]:
                del sys.modules[name]
            self.assertFalse(names & set(sys.modules))
            self.assertIsNone(checker.load_packaging())
            self.assertLessEqual(names, set(sys.modules))

    def test_packaging_is_loaded_before_the_probe_runs(self):
        order = []
        popen, _ = fake_popen(probe_result())

        def probe(*args, **options):
            order.append("probe")
            return popen(*args, **options)

        with patch.object(checker, "load_packaging", side_effect=lambda: order.append("packaging")):
            self.report("cpu-smoke", probe_result(), popen_override=probe)
        self.assertEqual(order, ["packaging", "probe"])
        self.assertIsNone(checker.load_packaging())
        self.assertLessEqual({"packaging.requirements", "packaging.tags", "packaging.utils", "packaging.version"}, set(sys.modules))

    def test_a_missing_lock_is_an_internal_error_with_the_install_remedy(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(checker, "lock_problems", side_effect=ImportError("packaging")):
            result, _ = self.report("cpu-smoke", probe_result(), clean=False, dependency_problems=lambda *args: [],
                                    site_problems=lambda *args: ([], []))
            versions = next(item for item in result["checks"] if item["name"] == "locked-versions")
            self.assertTrue(versions["problems"][0].endswith(worker.SYNC))
            missing = Path(directory) / "requirements.txt"
            with patch.object(checker, "LOCK", missing), patch.object(checker, "report", wraps=checker.report):
                code, output = self.main([])
        result = json.loads(output)
        self.assertEqual((code, result["status"], result["checks"]), (2, "rejected", []))
        self.assertTrue(result["error"].startswith("check could not run (InputError: requirements.txt: FileNotFoundError: "))
        self.assertTrue(result["error"].endswith("restore requirements.txt from the repository checkout"))

    def repository_input(self, relative, data):
        # Build a fake repository so the remedy shows the repository path of the input.
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = Path(directory.name)
        path = root / relative
        path.parent.mkdir(parents=True)
        if data is not None:
            path.write_bytes(data)
        return root, path

    def test_a_damaged_lock_is_an_internal_error_with_the_restore_remedy(self):
        for data in (b"\xff\xfe torch==2.14.0\n", b"packaging==not.a.version!\n"):
            with self.subTest(data=data):
                root, lock = self.repository_input("training/axolotl/requirements.txt", data)
                code, output = self.main([], LOCK=lock, HERE=root / "training/axolotl")
                result = json.loads(output)
                self.assertEqual((code, result["status"], result["checks"]), (2, "rejected", []))
                self.assertTrue(result["error"].startswith("check could not run (InputError: requirements.txt: "), result["error"])
                self.assertTrue(result["error"].endswith("restore training/axolotl/requirements.txt from the repository checkout"))

    def test_a_missing_worker_is_an_internal_error_with_the_restore_remedy(self):
        root, missing = self.repository_input("internal/llmtraining/worker.py", None)
        code, output = self.main([], WORKER=missing, HERE=root / "training/axolotl")
        result = json.loads(output)
        self.assertEqual((code, result["target"], result["checks"]), (2, "cpu-smoke", []))
        self.assertTrue(result["error"].startswith("check could not run (InputError: worker.py: FileNotFoundError: "))
        self.assertTrue(result["error"].endswith("restore internal/llmtraining/worker.py from the repository checkout"))

    def test_the_probe_gets_a_private_copy_of_the_hashed_worker_bytes(self):
        seen = []

        def popen(command, **options):
            path = Path(command[5])
            seen.append((path.parent == Path(options["cwd"]), path.name, path.read_bytes(), path.stat().st_mode & 0o777))
            Path(command[-1]).write_text(json.dumps(probe_result()))
            return FakeProcess((0,))

        self.assertEqual(checker.run_probe(False, None, b"hashed worker bytes", popen, lambda *args: None), (probe_result(), None))
        self.assertEqual(seen, [(True, "worker.py", b"hashed worker bytes", 0o600)])
        seen.clear()
        result, _ = self.report("cpu-smoke", probe_result(), popen_override=popen)
        self.assertEqual(seen[0][2], SOURCE)
        self.assertEqual(result["workerSHA256"], hashlib.sha256(SOURCE).hexdigest())

    def test_a_damaged_worker_is_an_internal_error_with_the_restore_remedy(self):
        for data in (b"def broken(:\n", b"<<<<<<< HEAD\nSYNC = 1\n=======\n", b"value = '\xff\xfe'\n", b"\x00"):
            with self.subTest(data=data):
                root, damaged = self.repository_input("internal/llmtraining/worker.py", data)
                code, output = self.main([], WORKER=damaged, HERE=root / "training/axolotl")
                result = json.loads(output)
                self.assertEqual((code, result["checks"]), (2, []))
                self.assertTrue(result["error"].startswith("check could not run (InputError: worker.py: "), result["error"])
                self.assertTrue(result["error"].endswith("restore internal/llmtraining/worker.py from the repository checkout"))

    def test_the_loader_runs_the_bytes_it_read_not_the_file(self):
        minimal = "SYNC = 'sync'\ndef runtime_site():\n    return None\n"
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "worker.py"
            path.write_text("X = 1\n" + minimal)
            hashed = ("X = 2\n" + minimal).encode()
            with patch.object(checker, "WORKER", path), patch.object(checker, "read_input", return_value=hashed):
                loaded, source = checker.load_worker()
        self.assertEqual((loaded.X, source), (2, hashed))

    def test_main_passes_the_loaded_bytes_to_the_report(self):
        with patch.object(checker, "load_worker", return_value=(worker, b"sentinel")), \
                patch.object(checker, "report", return_value={"status": "accepted"}) as report:
            code, _ = self.main([])
        self.assertEqual((code, report.call_args.kwargs["worker_source"]), (0, b"sentinel"))

    def test_a_worker_import_failure_is_a_checker_bug(self):
        root, failing = self.repository_input("internal/llmtraining/worker.py", b"import missing_module_xyz\n")
        code, output = self.main([], WORKER=failing, HERE=root / "training/axolotl")
        result = json.loads(output)
        self.assertEqual((code, result["checks"]), (2, []))
        self.assertTrue(result["error"].startswith("check could not run (ModuleNotFoundError: "), result["error"])
        self.assertTrue(result["error"].endswith(checker.CHECKER_BUG))

    def test_the_worker_hash_uses_the_loaded_bytes(self):
        loaded, source = checker.load_worker()
        self.assertEqual(source, checker.WORKER.read_bytes())
        self.assertEqual((loaded.SOURCE_REVISION, loaded.__file__), (worker.SOURCE_REVISION, str(checker.WORKER)))
        result, _ = self.report("cpu-smoke", probe_result())
        self.assertEqual(result["workerSHA256"], checker.digest(source))
        popen, _ = fake_popen(probe_result())
        with patch.object(checker.metadata, "distributions", return_value=[]):
            result = checker.report(worker, "cpu-smoke", None, popen, worker_source=b"other bytes")
        self.assertEqual(result["workerSHA256"], hashlib.sha256(b"other bytes").hexdigest())
        with self.assertRaises(TypeError):
            checker.report(worker, "cpu-smoke", None, popen)

    def test_report_without_a_probe_result_has_null_runtime_fields(self):
        result, _ = self.report("cpu-smoke", None)
        self.assertEqual([result[key] for key in ("accelerator", "torch", "cuda", "driverCuda", "archList", "devices")], [None] * 6)
        self.assertEqual(result["status"], "rejected")

        def explode(*args, **options):
            raise OSError("no temporary directory")

        with patch.object(checker.metadata, "distributions", return_value=[]):
            result = checker.report(worker, "cpu-smoke", None, explode, worker_source=SOURCE)
        native = next(item for item in result["checks"] if item["name"] == "native-libraries")
        self.assertEqual(native["problems"], [f"the runtime probe could not start (OSError: no temporary directory); {worker.SYNC}"])

        def broken(*args, **options):
            raise RuntimeError("unexpected")

        with patch.object(checker.metadata, "distributions", return_value=[]):
            result = checker.report(worker, "cpu-smoke", None, broken, worker_source=SOURCE)
        native = next(item for item in result["checks"] if item["name"] == "native-libraries")
        self.assertEqual(native["problems"], [f"the runtime probe could not start (RuntimeError: unexpected); {checker.CHECKER_BUG}"])

    def test_a_missing_packaging_module_is_an_installation_problem(self):
        def missing():
            raise ImportError("No module named 'packaging'")

        self.assertEqual(checker.guarded("locked-versions", missing, "cpu-smoke", worker.SYNC)["problems"],
                         [f"check could not run (ImportError: No module named 'packaging'); {worker.SYNC}"])
        with patch.object(checker, "report", side_effect=ImportError("No module named 'packaging'")):
            code, output = self.main([])
        self.assertEqual((code, json.loads(output)["error"]),
                         (2, f"check could not run (ImportError: No module named 'packaging'); {worker.SYNC}"))

    def test_check_writes_no_bytecode_in_an_isolated_runtime(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory) / "runtime"
            subprocess.run([sys.executable, "-m", "venv", "--without-pip", str(runtime)], check=True, capture_output=True)
            site = next(runtime.glob("lib/python*/site-packages"))
            shutil.copytree(Path(importlib.util.find_spec("packaging").origin).parent, site / "packaging",
                            ignore=shutil.ignore_patterns("__pycache__"))
            # packaging.tags imports logging; a planted copy must not run in the checker.
            marker = Path(directory) / "imported"
            (site / "logging.py").write_text(f"import sys\nopen({str(marker)!r}, 'a').write(sys.argv[0] + '\\n')\nraise SystemExit(9)\n")
            # A planted _sysconfigdata copy must neither run in the checker nor hide.
            name = subprocess.run([str(runtime / "bin/python3"), "-I", "-c", "import sysconfig; print(sysconfig._get_sysconfigdata_name())"],
                                  check=True, capture_output=True, text=True).stdout.strip()
            planted = [name, "logging"]
            (site / f"{name}.py").write_text("raise RuntimeError('planted sysconfig data')\n")
            dynload = Path(sysconfig.get_paths()["stdlib"]) / "lib-dynload"
            private = sorted(module for module in (checker.module_name(entry) for entry in dynload.iterdir()) if module
                             and module.startswith("_") and module not in sys.stdlib_module_names) if dynload.is_dir() else []
            if private:
                (site / f"{private[0]}.py").write_text("raise RuntimeError('planted dynload copy')\n")
                planted.append(private[0])
            before = sorted(path.relative_to(runtime) for path in runtime.rglob("*"))
            environment = {key: value for key, value in os.environ.items() if key != "PYTHONDONTWRITEBYTECODE"}
            completed = subprocess.run([str(runtime / "bin/python3"), "-I", "-S", str(ROOT / "training/axolotl/check_environment.py")],
                                       env=environment, capture_output=True, check=False, timeout=120)
            self.assertEqual(completed.returncode, 1, completed.stderr)
            result = json.loads(completed.stdout)
            self.assertEqual(set(result), STABLE_REPORT_KEYS | {"error"})
            self.assertEqual(sorted(path.relative_to(runtime) for path in runtime.rglob("*")), before)
            imported = marker.read_text().splitlines() if marker.exists() else []
            self.assertFalse([line for line in imported if line.endswith("check_environment.py")])
            standard = next(item for item in result["checks"] if item["name"] == "standard-library")
            for module in planted:
                self.assertIn(f"site-packages has {module}.py, which shadows the standard-library module {module}; remove it",
                              standard["problems"])
            versions = " ".join(next(item for item in result["checks"] if item["name"] == "locked-versions")["problems"])
            self.assertNotIn("check could not run", versions)
            self.assertNotIn("not a wheel for this interpreter", versions)


STABLE_REPORT_KEYS = {"version", "target", "status", "python", "lockTarget", "workerSHA256", "lockSHA256",
                      "accelerator", "torch", "cuda", "driverCuda", "archList", "devices", "checks"}


class ProbeTests(unittest.TestCase):
    def test_native_standard_library_modules_load(self):
        self.assertEqual(probe.stdlib_problems(("json", "sys")), [])
        self.assertEqual(probe.stdlib_problems(("absent_stdlib_module",)), [
            "standard-library module absent_stdlib_module cannot load; rebuild or reinstall CPython with its system libraries"])
        with patch.object(probe.importlib, "import_module", side_effect=OSError("libssl.so.3")):
            self.assertEqual(probe.stdlib_problems(("json",)), [
                "standard-library module json cannot load; rebuild or reinstall CPython with its system libraries"])

    def test_bitsandbytes_library_and_package_load_for_the_torch_cuda_build(self):
        with tempfile.TemporaryDirectory() as directory:
            package = Path(directory)
            spec = SimpleNamespace(origin=str(package / "__init__.py"))
            with patch.object(probe.importlib.util, "find_spec", return_value=spec):
                self.assertEqual(probe.bitsandbytes_problem("13.0"), "bitsandbytes has no library for CUDA 13.0; install the bitsandbytes wheel from the lock")
                (package / "libbitsandbytes_cuda130.so").write_text("not a library")
                self.assertTrue(probe.bitsandbytes_problem("13.0").startswith("the bitsandbytes CUDA 13.0 library cannot load (OSError: "))
                with patch.object(probe.ctypes, "CDLL") as load, patch.object(probe.importlib, "import_module") as imported:
                    self.assertIsNone(probe.bitsandbytes_problem("13.0"))
                    load.assert_called_once_with(str(package / "libbitsandbytes_cuda130.so"))
                    imported.assert_called_once_with("bitsandbytes")
                    imported.side_effect = RuntimeError("CUDA setup failed")
                    self.assertEqual(probe.bitsandbytes_problem("13.0"),
                                     "bitsandbytes cannot import (RuntimeError: CUDA setup failed); reinstall bitsandbytes from the lock")
            with patch.object(probe.importlib.util, "find_spec", return_value=None):
                self.assertIsNone(probe.bitsandbytes_problem("13.0"))

    def test_probe_inserts_the_runtime_site_and_reports_torch_failures(self):
        with tempfile.TemporaryDirectory() as directory:
            def fail():
                raise RuntimeError("CUDA initialization failed\nmore")

            fake = SimpleNamespace(runtime_site=lambda: Path(directory), torch_facts=fail)
            with patch.object(sys, "path", list(sys.path)):
                result = probe.probe(fake, True)
                self.assertEqual(sys.path[0], directory)
            self.assertEqual(result, {"stdlib": [], "facts": None, "torchError": "RuntimeError: CUDA initialization failed", "bitsandbytes": None})
            fake.torch_facts = cuda_facts
            with patch.object(sys, "path", list(sys.path)), patch.object(probe, "bitsandbytes_problem", return_value="missing") as library:
                self.assertEqual(probe.probe(fake, True)["bitsandbytes"], "missing")
                library.assert_called_once_with("13.0")
                self.assertIsNone(probe.probe(fake, False)["bitsandbytes"])
                fake.torch_facts = lambda: cuda_facts(cuda=None)
                self.assertIsNone(probe.probe(fake, True)["bitsandbytes"])
                library.assert_called_once()


if __name__ == "__main__":
    unittest.main()
