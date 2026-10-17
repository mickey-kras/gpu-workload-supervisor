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


def module(name: str, attributes: dict[str, object]) -> ModuleType:
    result = ModuleType(name)
    for key, value in attributes.items():
        setattr(result, key, value)
    return result


class WorkerTests(unittest.TestCase):
    def test_only_pinned_noneditable_runtime_is_accepted(self) -> None:
        for metadata in (
            {},
            {"vcs_info": {"commit_id": "changed"}},
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
        for group, value, owner in (
            ("axolotl.plugins", "untrusted:Plugin", "axolotl"),
            ("axolotl.models", "untrusted:Model", "axolotl"),
            ("axolotl.plugins", next(iter(worker.BUILTIN_PLUGINS)), "another-package"),
        ):
            point = SimpleNamespace(group=group, value=value, dist=SimpleNamespace(metadata={"Name": owner}))
            with self.subTest(group=group, owner=owner), patch.object(
                worker.importlib.metadata,
                "distribution",
                return_value=SimpleNamespace(read_text=lambda _: json.dumps(approved)),
            ), patch.object(worker.importlib.metadata, "entry_points", return_value=[point]):
                with self.assertRaises(ValueError):
                    worker.verify_environment()

    def test_bundled_plugins_and_unrelated_entry_points_are_accepted(self) -> None:
        approved = {"vcs_info": {"commit_id": worker.SOURCE_REVISION}}
        points = [SimpleNamespace(group="axolotl.plugins", value=value, dist=SimpleNamespace(metadata={"Name": "axolotl"})) for value in worker.BUILTIN_PLUGINS]
        points.append(SimpleNamespace(group="console_scripts", value="other:command", dist=None))
        with patch.object(worker.importlib.metadata, "distribution", return_value=SimpleNamespace(read_text=lambda _: json.dumps(approved))), patch.object(worker.importlib.metadata, "entry_points", return_value=points):
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
                cfg = SimpleNamespace(max_steps=2, output_dir=directory)

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
                with patch.dict(worker.sys.modules, modules):
                    if steps == 2 and loss == 1.0:
                        worker.train(root / "config.json", receipt, root / "progress.json")
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
                tokens: MagicMock = MagicMock()
                tokens.to.return_value = tokens
                tokens.__getitem__.return_value.shape = (1, 1)
                tokenizer: MagicMock = MagicMock()
                tokenizer.from_pretrained.return_value.return_value = tokens
                peft: MagicMock = MagicMock()
                peft.from_pretrained.return_value = model
                modules = {
                    "torch": torch,
                    "peft": module("peft", {"PeftModel": peft}),
                    "transformers": module("transformers", {"AutoModelForCausalLM": MagicMock(), "AutoTokenizer": tokenizer, "BitsAndBytesConfig": MagicMock()}),
                }
                receipt = root / "receipt.json"
                with patch.dict(worker.sys.modules, modules):
                    if finite and length > 1:
                        worker.reload(config, receipt)
                        self.assertTrue(json.loads(receipt.read_text())["reloaded"])
                        tokenizer.from_pretrained.assert_called_once_with("saved", local_files_only=True, trust_remote_code=False)
                        self.assertEqual(peft.from_pretrained.call_args.args[1], "saved")
                        model.generate.assert_called_once_with(max_new_tokens=8, do_sample=False)
                    else:
                        with self.assertRaises(ValueError):
                            worker.reload(config, receipt)
                        self.assertFalse(receipt.exists())

    def test_main_selects_only_the_requested_phase_after_verification(self) -> None:
        for phase in ("train", "reload"):
            previous = os.umask(0o077)
            try:
                with patch.object(worker.sys, "path", list(worker.sys.path)), patch.object(worker, "verify_environment") as verify, patch.object(worker, phase) as action:
                    worker.main([phase, "config", "receipt", "progress"])
                    verify.assert_called_once_with()
                    action.assert_called_once()
            finally:
                os.umask(previous)


if __name__ == "__main__":
    unittest.main()
