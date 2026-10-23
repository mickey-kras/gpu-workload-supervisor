from __future__ import annotations

import importlib.util
import json
from contextlib import nullcontext
import os
from pathlib import Path
import tempfile
from types import ModuleType, SimpleNamespace
import unittest
from unittest.mock import MagicMock, patch

WORKER = Path(__file__).resolve().parents[2] / "internal/llmtraining/worker.py"
SPEC = importlib.util.spec_from_file_location("training_worker", WORKER)
assert SPEC is not None and SPEC.loader is not None
worker = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(worker)

# Complete entry-point metadata declared by the pinned Axolotl pyproject.toml.
PINNED_ENTRY_POINTS = """[console_scripts]
axolotl = axolotl.cli.main:main

[axolotl.cloud_providers]
coreweave = axolotl.integrations.coreweave.cloud:CoreWeaveCloud
nebius = axolotl.integrations.nebius.cloud:NebiusCloud
modal = axolotl.integrations.modal.cloud:ModalCloud
baseten = axolotl.integrations.baseten.cloud:BasetenCloud

[axolotl.plugins]
context-parallel = axolotl.integrations.context_parallel:ContextParallelPlugin
expert-parallel = axolotl.integrations.expert_parallel:ExpertParallelPlugin
"""


def module(name: str, attributes: dict[str, object]) -> ModuleType:
    result = ModuleType(name)
    for key, value in attributes.items():
        setattr(result, key, value)
    return result


class WorkerTests(unittest.TestCase):
    def test_only_pinned_noneditable_runtime_is_accepted(self) -> None:
        for metadata in (
            [],
            {},
            {"vcs_info": None},
            {"vcs_info": []},
            {"vcs_info": {"commit_id": "changed"}},
            {"vcs_info": {"commit_id": worker.SOURCE_REVISION}, "dir_info": None},
            {"vcs_info": {"commit_id": worker.SOURCE_REVISION}, "dir_info": []},
            {"vcs_info": {"commit_id": worker.SOURCE_REVISION}, "dir_info": {"editable": True}},
        ):
            with self.subTest(metadata=metadata), patch.object(
                worker.importlib.metadata,
                "distribution",
                return_value=SimpleNamespace(read_text=lambda _: json.dumps(metadata)),
            ), patch.object(worker.importlib.metadata, "entry_points", return_value=[]):
                with self.assertRaises(ValueError):
                    worker.verify_environment()

    def test_unapproved_entry_points_are_rejected_before_import(self) -> None:
        approved = {"vcs_info": {"commit_id": worker.SOURCE_REVISION}}
        for group, name, value, owner in (
            ("axolotl.plugins", "untrusted", "untrusted:Plugin", "axolotl"),
            ("axolotl.models", "untrusted", "untrusted:Model", "axolotl"),
            ("axolotl.plugins", "context-parallel", "axolotl.integrations.context_parallel:ContextParallelPlugin", "another-package"),
        ):
            point = SimpleNamespace(group=group, name=name, value=value, dist=SimpleNamespace(metadata={"Name": owner}))
            with self.subTest(group=group, owner=owner), patch.object(
                worker.importlib.metadata,
                "distribution",
                return_value=SimpleNamespace(read_text=lambda _: json.dumps(approved)),
            ), patch.object(worker.importlib.metadata, "entry_points", return_value=[point]):
                with self.assertRaises(ValueError):
                    worker.verify_environment()

    def test_bundled_plugins_and_unrelated_entry_points_are_accepted(self) -> None:
        approved = {"vcs_info": {"commit_id": worker.SOURCE_REVISION}}
        points = [SimpleNamespace(group=group, name=name, value=value, dist=SimpleNamespace(metadata={"Name": "axolotl"})) for group, entries in worker.BUILTIN_ENTRY_POINTS.items() for name, value in entries.items()]
        points.append(SimpleNamespace(group="console_scripts", value="other:command", dist=None))
        with patch.object(worker.importlib.metadata, "distribution", return_value=SimpleNamespace(read_text=lambda _: json.dumps(approved))), patch.object(worker.importlib.metadata, "entry_points", return_value=points):
            worker.verify_environment()

    def test_real_pinned_distribution_metadata_and_unapproved_registrations(self) -> None:
        additions = (
            None,
            ("axolotl", "axolotl.plugins", "context-parallel", "untrusted:Plugin"),
            ("axolotl", "axolotl.cloud_providers", "modal", "untrusted:Provider"),
            ("axolotl", "axolotl.cloud_providers", "unexpected", "axolotl.integrations.modal.cloud:ModalCloud"),
            ("axolotl", "axolotl.models", "modal", "axolotl.integrations.modal.cloud:ModalCloud"),
            ("another-package", "axolotl.plugins", "context-parallel", "axolotl.integrations.context_parallel:ContextParallelPlugin"),
            ("another-package", "axolotl.cloud_providers", "modal", "axolotl.integrations.modal.cloud:ModalCloud"),
        )
        for addition in additions:
            with self.subTest(addition=addition), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                pinned = root / "axolotl-0.0.0.dist-info"
                pinned.mkdir()
                (pinned / "METADATA").write_text("Metadata-Version: 2.1\nName: axolotl\nVersion: 0.0.0\nRequires-Python: >=3.12\n")
                (pinned / "entry_points.txt").write_text(PINNED_ENTRY_POINTS)
                (pinned / "direct_url.json").write_text(json.dumps({
                    "url": "https://github.com/axolotl-ai-cloud/axolotl.git",
                    "vcs_info": {"vcs": "git", "commit_id": worker.SOURCE_REVISION},
                }))
                if addition is not None:
                    owner, group, name, value = addition
                    if owner == "axolotl":
                        text = "\n".join(
                            f"{name} = {value}" if line.startswith(f"{name} = ") else line
                            for line in PINNED_ENTRY_POINTS.split("\n")
                        )
                        if text == PINNED_ENTRY_POINTS:
                            section = f"[{group}]\n"
                            if section in text:
                                text = text.replace(section, f"{section}{name} = {value}\n")
                            else:
                                text += f"\n{section}{name} = {value}\n"
                        (pinned / "entry_points.txt").write_text(text)
                    else:
                        third_party = root / "another_package-0.0.0.dist-info"
                        third_party.mkdir()
                        (third_party / "METADATA").write_text(f"Metadata-Version: 2.1\nName: {owner}\nVersion: 0.0.0\n")
                        (third_party / "entry_points.txt").write_text(f"[{group}]\n{name} = {value}\n")
                discover = worker.importlib.metadata.Distribution.discover
                with patch.object(worker.sys, "path", [directory, *worker.sys.path]), patch.object(
                    worker.importlib.metadata.Distribution, "discover",
                    side_effect=lambda **kwargs: discover(path=[directory], **kwargs),
                ), patch.object(
                    worker.importlib.metadata.EntryPoint, "load", side_effect=AssertionError("entry points must remain inert")
                ):
                    if addition is None:
                        self.assertEqual(len(worker.importlib.metadata.distribution("axolotl").entry_points), 7)
                        worker.verify_environment()
                    else:
                        with self.assertRaisesRegex(ValueError, "unapproved runtime entry point"):
                            worker.verify_environment()

    def test_progress_replaces_previous_record_without_accumulating_data(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "progress.json"
            worker.publish(path, {"version": 1, "step": 1})
            worker.publish(path, {"version": 1, "step": 2})
            self.assertEqual(json.loads(path.read_text()), {"version": 1, "step": 2})
            self.assertEqual([entry.name for entry in path.parent.iterdir()], ["progress.json"])

    def test_failed_publication_preserves_previous_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "progress.json"
            worker.publish(path, {"step": 1})
            with self.assertRaises(ValueError):
                worker.publish(path, {"step": float("nan")})
            self.assertEqual(json.loads(path.read_text()), {"step": 1})

    def test_worker_rejects_unsupported_invocation(self) -> None:
        for arguments in ([], ["resume", "a", "b", "c"], ["train", "a", "b"]):
            with self.subTest(arguments=arguments), self.assertRaises(ValueError):
                worker.main(arguments)

    def test_native_training_requires_requested_progress_and_valid_evaluation(self) -> None:
        for steps, loss in ((2, 1.0), (1, 1.0), (2, float("nan")), (2, -1.0)):
            with self.subTest(steps=steps, loss=loss), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                trainer: MagicMock = MagicMock()
                trainer.state.global_step = steps
                trainer.evaluate.return_value = {"eval_loss": loss}
                tokenizer: MagicMock = MagicMock()
                saved: MagicMock = MagicMock()
                cfg = SimpleNamespace(max_steps=2, output_dir=directory, load_in_4bit=False)

                def callback(value: object) -> None:
                    getattr(value, "on_step_end")(None, SimpleNamespace(global_step=steps), None)

                trainer.add_callback.side_effect = callback
                modules = {
                    "axolotl": module("axolotl", {}),
                    "axolotl.cli": module("axolotl.cli", {}),
                    "axolotl.cli.config": module("axolotl.cli.config", {"load_cfg": MagicMock(return_value=cfg)}),
                    "axolotl.common": module("axolotl.common", {}),
                    "axolotl.common.datasets": module("axolotl.common.datasets", {"load_datasets": MagicMock()}),
                    "axolotl.train": module("axolotl.train", {
                        "save_initial_configs": MagicMock(),
                        "save_trained_model": saved,
                        "setup_model_and_trainer": MagicMock(return_value=(trainer, object(), tokenizer, None, None)),
                    }),
                    "transformers": module("transformers", {
                        "TrainerCallback": object, "TrainerControl": object,
                        "TrainerState": object, "TrainingArguments": object,
                    }),
                }
                receipt = root / "receipt.json"
                with patch.dict(worker.sys.modules, modules), patch.object(worker, "verify_accelerator") as accelerator:
                    if steps == 2 and loss == 1.0:
                        worker.train(root / "config.json", receipt, root / "progress.json")
                        accelerator.assert_called_once_with(False)
                        self.assertEqual(json.loads(receipt.read_text())["step"], 2)
                        trainer.train.assert_called_once_with(resume_from_checkpoint=None)
                        trainer.evaluate.assert_called_once_with()
                        saved.assert_called_once()
                        self.assertEqual(json.loads((root / "progress.json").read_text())["step"], 2)
                        self.assertEqual(receipt.stat().st_mode & 0o777, 0o600)
                    else:
                        with self.assertRaises(ValueError):
                            worker.train(root / "config.json", receipt, root / "progress.json")
                        self.assertFalse(receipt.exists())
                        saved.assert_not_called()

    def test_reload_uses_saved_adapter_and_tokenizer_and_requires_real_output(self) -> None:
        for quantized, finite, length in ((False, True, 9), (True, True, 9), (True, False, 9), (True, True, 1)):
            with self.subTest(quantized=quantized, finite=finite, length=length), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                cfg = {"base_model": "base", "tokenizer_config": "original", "output_dir": "saved", "load_in_4bit": quantized, "max_steps": 2}
                config = root / "config.json"
                config.write_text(json.dumps(cfg))
                torch: MagicMock = MagicMock()
                torch.inference_mode.return_value = nullcontext()
                torch.isfinite.return_value.all.return_value.item.return_value = finite
                model: MagicMock = MagicMock()
                model.generate.return_value = SimpleNamespace(shape=(1, length))
                input_ids = SimpleNamespace(shape=(1, 1))
                tokens = {"input_ids": input_ids}
                tokenizer: MagicMock = MagicMock()
                tokenizer.from_pretrained.return_value.return_value.to.return_value = tokens
                peft: MagicMock = MagicMock()
                peft.from_pretrained.return_value = model
                modules = {
                    "torch": torch,
                    "peft": module("peft", {"PeftModel": peft}),
                    "transformers": module("transformers", {"AutoModelForCausalLM": MagicMock(), "AutoTokenizer": tokenizer, "BitsAndBytesConfig": MagicMock()}),
                }
                receipt = root / "receipt.json"
                with patch.dict(worker.sys.modules, modules), patch.object(worker, "verify_accelerator") as accelerator:
                    if finite and length > 1:
                        worker.reload(config, receipt)
                        accelerator.assert_called_once_with(quantized)
                        self.assertTrue(json.loads(receipt.read_text())["reloaded"])
                        tokenizer.from_pretrained.assert_called_once_with("saved", local_files_only=True, trust_remote_code=False)
                        self.assertEqual(peft.from_pretrained.call_args.args[1], "saved")
                        model.assert_called_once_with(input_ids=input_ids)
                        model.generate.assert_called_once_with(input_ids=input_ids, max_new_tokens=8, do_sample=False)
                    else:
                        with self.assertRaises(ValueError):
                            worker.reload(config, receipt)
                        self.assertFalse(receipt.exists())

    def test_main_rejects_unsupported_python_before_runtime_preparation(self) -> None:
        for phase in ("train", "reload"):
            with self.subTest(phase=phase), patch.object(
                worker.sys, "version_info", SimpleNamespace(major=3, minor=11)
            ), patch.object(worker.sys, "path", []) as paths, patch.object(
                worker, "verify_environment"
            ) as verify, patch.object(worker, "train") as train, patch.object(worker, "reload") as reload:
                with self.assertRaisesRegex(ValueError, "Python 3.12 or newer"):
                    worker.main([phase, "config", "receipt", "progress"])
                self.assertEqual(paths, [])
                verify.assert_not_called()
                train.assert_not_called()
                reload.assert_not_called()

    def test_main_selects_only_the_requested_phase_after_verification(self) -> None:
        for phase in ("train", "reload"):
            previous = os.umask(0o077)
            try:
                with patch.object(
                    worker.sys, "version_info", SimpleNamespace(major=3, minor=12)
                ), patch.object(worker.sys, "path", list(worker.sys.path)) as paths, patch.object(
                    worker, "verify_environment"
                ) as verify, patch.object(worker, "train") as train, patch.object(worker, "reload") as reload:
                    worker.main([phase, "config", "receipt", "progress"])
                    verify.assert_called_once_with()
                    self.assertEqual(paths[0], str(worker.runtime_site()))
                    if phase == "train":
                        train.assert_called_once_with(Path("config"), Path("receipt"), Path("progress"))
                        reload.assert_not_called()
                    else:
                        reload.assert_called_once_with(Path("config"), Path("receipt"))
                        train.assert_not_called()
            finally:
                os.umask(previous)

    def test_phases_check_the_accelerator_before_training_or_loading(self) -> None:
        load_cfg: MagicMock = MagicMock(return_value=SimpleNamespace(load_in_4bit=False))
        load_datasets: MagicMock = MagicMock()
        modules = {
            "axolotl": module("axolotl", {}), "axolotl.cli": module("axolotl.cli", {}),
            "axolotl.cli.config": module("axolotl.cli.config", {"load_cfg": load_cfg}),
            "axolotl.common": module("axolotl.common", {}),
            "axolotl.common.datasets": module("axolotl.common.datasets", {"load_datasets": load_datasets}),
            "axolotl.train": module("axolotl.train", {
                "save_initial_configs": MagicMock(), "save_trained_model": MagicMock(), "setup_model_and_trainer": MagicMock(),
            }),
            "transformers": module("transformers", {
                "TrainerCallback": object, "TrainerControl": object, "TrainerState": object, "TrainingArguments": object,
            }),
        }
        with tempfile.TemporaryDirectory() as directory, patch.dict(worker.sys.modules, modules), patch.object(
            worker, "torch_facts", return_value=accelerator_facts(cuda=None)
        ):
            root = Path(directory)
            with self.assertRaisesRegex(ValueError, "unsupported training accelerator: Torch 2.14.0 is not a CUDA build"):
                worker.train(root / "config.json", root / "receipt.json", root / "progress.json")
            load_datasets.assert_not_called()
            for value in (None, 1, "true"):
                load_cfg.return_value = SimpleNamespace(load_in_4bit=value)
                with self.subTest(value=value), self.assertRaisesRegex(ValueError, "load_in_4bit must be a boolean"):
                    worker.train(root / "config.json", root / "receipt.json", root / "progress.json")
            load_cfg.return_value = SimpleNamespace(load_in_4bit=None)
            load_datasets.side_effect = RuntimeError("datasets reached")
            with patch.object(worker, "verify_accelerator") as accelerator, self.assertRaisesRegex(RuntimeError, "datasets reached"):
                worker.train(root / "config.json", root / "receipt.json", root / "progress.json", accelerator=False)
            accelerator.assert_not_called()
            config = root / "config.json"
            for value, expected in ((False, "not a CUDA build"), ("yes", "load_in_4bit must be a boolean")):
                config.write_text(json.dumps({"load_in_4bit": value}))
                torch: MagicMock = MagicMock()
                with self.subTest(value=value), patch.dict(worker.sys.modules, {"torch": torch}), self.assertRaisesRegex(ValueError, expected):
                    worker.reload(config, root / "receipt.json")
                self.assertFalse((root / "receipt.json").exists())


def accelerator_facts(**changes: object) -> dict[str, object]:
    facts: dict[str, object] = {
        "torch": "2.14.0", "cuda": "13.0", "hip": None, "available": True, "driverCuda": "13.0",
        "archList": ["sm_75", "sm_80", "sm_86", "sm_90", "sm_100", "sm_120", "compute_120"],
        "devices": [[8, 6]], "bitsandbytes": True,
    }
    facts.update(changes)
    return facts


class AcceleratorTests(unittest.TestCase):
    def test_kernel_support_follows_sass_and_ptx_compatibility(self) -> None:
        for capability, arch_list, expected in (
            ((8, 6), ["sm_80"], "sass"),
            ((8, 9), ["sm_86"], "sass"),
            ((8, 0), ["sm_86"], ""),
            ((9, 0), ["sm_80"], ""),
            ((9, 0), ["sm_80", "compute_80"], "ptx"),
            ((12, 0), ["sm_90", "sm_100"], ""),
            ((12, 1), ["sm_120", "compute_120"], "sass"),
            ((9, 0), ["sm_90a"], "sass"),
            ((10, 0), ["sm_90a", "compute_90a"], ""),
            ((10, 3), ["sm_100f"], "sass"),
            ((12, 0), ["compute_100f"], ""),
            ((12, 0), ["compute_100"], "ptx"),
            ((8, 6), ["unknown", "sm_x"], ""),
        ):
            with self.subTest(capability=capability, arch_list=arch_list):
                self.assertEqual(worker.kernel_support(capability, arch_list), expected)

    def test_supported_cuda_device_has_no_problems(self) -> None:
        self.assertEqual(worker.accelerator_problems(accelerator_facts(), True), [])
        self.assertEqual(worker.accelerator_problems(accelerator_facts(devices=[[13, 0]], driverCuda="13.1"), False), [])
        # BF16 starts at 8.0, and PTX needs a driver equal to or newer than the build.
        self.assertEqual(worker.accelerator_problems(accelerator_facts(devices=[[8, 0]]), False), [])
        self.assertEqual(worker.accelerator_problems(accelerator_facts(devices=[[13, 0]], driverCuda="13.0"), False), [])

    def test_each_visible_device_is_checked_by_index(self) -> None:
        facts = accelerator_facts(devices=[[8, 6], [12, 0]], archList=["sm_80", "sm_86"])
        self.assertEqual(worker.accelerator_problems(facts, False), [
            "GPU 1 (compute capability 12.0) has no kernels in Torch 2.14.0 (sm_80 sm_86); "
            "install a Torch build with sm_120 or use a supported GPU"])
        self.assertEqual(worker.accelerator_problems(accelerator_facts(devices=[[8, 6], [7, 5]]), False), [
            "GPU 1 (compute capability 7.5) has no native BF16, which the training presets need; "
            "use a GPU with compute capability 8.0 or newer"])
        self.assertEqual(worker.accelerator_problems(accelerator_facts(archList=[]), False), [
            "GPU 0 (compute capability 8.6) has no kernels in Torch 2.14.0 (an empty architecture list); "
            "install a Torch build with sm_86 or use a supported GPU"])

    def test_python_rule_and_runtime_site_match_the_runner(self) -> None:
        self.assertIsNone(worker.python_problem((3, 12)))
        self.assertIsNone(worker.python_problem((3, 13)))
        self.assertEqual(worker.python_problem((3, 11)), "training runtime requires Python 3.12 or newer")
        with patch.object(worker.sys, "executable", "/runtime/bin/python3"):
            version = f"python{worker.sys.version_info.major}.{worker.sys.version_info.minor}"
            self.assertEqual(worker.runtime_site(), Path("/runtime/lib") / version / "site-packages")

    def test_unsupported_combinations_have_actionable_problems(self) -> None:
        for changes, quantized, expected in (
            ({"cuda": None}, False, ["Torch 2.14.0 is not a CUDA build; install the CUDA torch wheel from the training lock"]),
            ({"hip": "7.0"}, False, ["Torch 2.14.0 is not a CUDA build; install the CUDA torch wheel from the training lock"]),
            ({"available": False}, False, ["Torch CUDA 13.0 cannot use a GPU; install an NVIDIA driver for CUDA 13.0 or newer and expose one GPU"]),
            ({"devices": []}, False, ["Torch CUDA 13.0 cannot use a GPU; install an NVIDIA driver for CUDA 13.0 or newer and expose one GPU"]),
            ({"driverCuda": "12.8"}, False, ["the NVIDIA driver supports CUDA 12.8, older than the Torch CUDA 13.0 build; upgrade the driver"]),
            ({"archList": ["sm_90", "sm_100"]}, False, ["GPU 0 (compute capability 8.6) has no kernels in Torch 2.14.0 (sm_90 sm_100); install a Torch build with sm_86 or use a supported GPU"]),
            ({"devices": [[13, 0]], "driverCuda": None}, False, ["GPU 0 (compute capability 13.0) needs PTX JIT compilation, which needs a driver for CUDA 13.0 or newer; upgrade the driver"]),
            ({"cuda": "13.2", "devices": [[13, 0]]}, False, ["GPU 0 (compute capability 13.0) needs PTX JIT compilation, which needs a driver for CUDA 13.2 or newer; upgrade the driver"]),
            ({"devices": [[7, 5]]}, False, ["GPU 0 (compute capability 7.5) has no native BF16, which the training presets need; use a GPU with compute capability 8.0 or newer"]),
            ({"bitsandbytes": False}, True, ["QLoRA needs bitsandbytes; install bitsandbytes from the training lock"]),
        ):
            with self.subTest(changes=changes, quantized=quantized):
                self.assertEqual(worker.accelerator_problems(accelerator_facts(**changes), quantized), expected)
        self.assertEqual(worker.accelerator_problems(accelerator_facts(bitsandbytes=False), False), [])

    def test_torch_facts_report_cpu_and_cuda_runtimes_without_initializing_cpu_builds(self) -> None:
        for cuda, available in ((None, True), ("13.0", False), ("13.0", True)):
            torch: MagicMock = MagicMock()
            torch.__version__ = "2.14.0"
            torch.version = SimpleNamespace(cuda=cuda, hip=None)
            torch.cuda.is_available.return_value = available
            torch.cuda.get_arch_list.return_value = ["sm_86"]
            torch.cuda.device_count.return_value = 2
            torch.cuda.get_device_capability.side_effect = lambda index: (8, 6 + index)
            with self.subTest(cuda=cuda, available=available), patch.dict(worker.sys.modules, {"torch": torch}), patch.object(
                worker, "driver_cuda", return_value="13.1"
            ), patch.object(worker.importlib.util, "find_spec", return_value=None):
                facts = worker.torch_facts()
                self.assertEqual((facts["torch"], facts["cuda"], facts["bitsandbytes"]), ("2.14.0", cuda, False))
                if cuda and available:
                    self.assertEqual((facts["available"], facts["driverCuda"], facts["archList"], facts["devices"]), (True, "13.1", ["sm_86"], [[8, 6], [8, 7]]))
                else:
                    self.assertEqual((facts["available"], facts["driverCuda"], facts["devices"]), (False, None, []))
                if cuda is None:
                    torch.cuda.is_available.assert_not_called()

    def test_driver_version_comes_from_the_driver_library(self) -> None:
        def get_version(reference: object) -> int:
            getattr(reference, "_obj").value = 13010
            return 0

        def version(value: int) -> object:
            def get(reference: object) -> int:
                getattr(reference, "_obj").value = value
                return 0
            return get

        for library, expected in (
            (SimpleNamespace(cuDriverGetVersion=get_version), "13.1"),
            (SimpleNamespace(cuDriverGetVersion=version(12080)), "12.8"),
            (SimpleNamespace(cuDriverGetVersion=version(13020)), "13.2"),
            (SimpleNamespace(cuDriverGetVersion=lambda reference: 100), None),
            (SimpleNamespace(), None),
        ):
            with self.subTest(expected=expected), patch.object(worker.ctypes, "CDLL", return_value=library):
                self.assertEqual(worker.driver_cuda(), expected)
        with patch.object(worker.ctypes, "CDLL", side_effect=OSError("missing")):
            self.assertIsNone(worker.driver_cuda())

    def test_verify_accelerator_raises_every_problem(self) -> None:
        with patch.object(worker, "torch_facts", return_value=accelerator_facts(devices=[[7, 5]], bitsandbytes=False)):
            with self.assertRaisesRegex(ValueError, "no native BF16.*; QLoRA needs bitsandbytes"):
                worker.verify_accelerator(True)
        with patch.object(worker, "torch_facts", return_value=accelerator_facts()):
            worker.verify_accelerator(True)


if __name__ == "__main__":
    unittest.main()
