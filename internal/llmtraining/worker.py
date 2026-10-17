"""Pinned Axolotl experiment worker; invoked only by the supervised runner."""

from __future__ import annotations

import importlib.metadata
import json
import math
import os
from pathlib import Path
import sys

SOURCE_REVISION = "1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1"
BUILTIN_ENTRY_POINTS = {
    "axolotl.plugins": {
        "context-parallel": "axolotl.integrations.context_parallel:ContextParallelPlugin",
        "expert-parallel": "axolotl.integrations.expert_parallel:ExpertParallelPlugin",
    },
    "axolotl.cloud_providers": {
        "coreweave": "axolotl.integrations.coreweave.cloud:CoreWeaveCloud",
        "nebius": "axolotl.integrations.nebius.cloud:NebiusCloud",
        "modal": "axolotl.integrations.modal.cloud:ModalCloud",
        "baseten": "axolotl.integrations.baseten.cloud:BasetenCloud",
    },
}


def verify_environment() -> None:
    distribution = importlib.metadata.distribution("axolotl")
    direct_url = json.loads(distribution.read_text("direct_url.json") or "{}")
    if not isinstance(direct_url, dict):
        raise ValueError("runtime source is not pinned")
    vcs_info = direct_url.get("vcs_info")
    dir_info = direct_url.get("dir_info", {})
    if (
        not isinstance(vcs_info, dict)
        or vcs_info.get("commit_id") != SOURCE_REVISION
        or not isinstance(dir_info, dict)
        or dir_info.get("editable", False)
    ):
        raise ValueError("runtime source is not pinned")
    for point in importlib.metadata.entry_points():
        if point.group.startswith("axolotl.") and (
            point.group not in BUILTIN_ENTRY_POINTS
            or BUILTIN_ENTRY_POINTS[point.group].get(point.name) != point.value
            or point.dist is None
            or point.dist.metadata["Name"].lower() != "axolotl"
        ):
            raise ValueError("unapproved runtime entry point")


def publish(path: Path, value: dict[str, object]) -> None:
    temporary = path.with_suffix(".pending")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as output:
        json.dump(value, output, allow_nan=False)
        output.flush()
        os.fsync(output.fileno())
    temporary.replace(path)


def train(config_path: Path, receipt: Path, progress: Path) -> None:
    from axolotl.cli.config import load_cfg
    from axolotl.common.datasets import load_datasets
    from axolotl.train import (
        save_initial_configs,
        save_trained_model,
        setup_model_and_trainer,
    )
    from transformers import TrainerCallback, TrainerControl, TrainerState, TrainingArguments

    class Progress(TrainerCallback):
        def on_step_end(
            self,
            args: TrainingArguments,
            state: TrainerState,
            control: TrainerControl,
            **kwargs: object,
        ) -> TrainerControl:
            publish(progress, {"version": 1, "phase": "training", "step": state.global_step})
            return control

    cfg = load_cfg(str(config_path))
    dataset = load_datasets(cfg=cfg)
    trainer, model, tokenizer, adapter, processor = setup_model_and_trainer(cfg, dataset)
    save_initial_configs(cfg, tokenizer, model, adapter, processor)
    trainer.add_callback(Progress())
    trainer.train(resume_from_checkpoint=None)
    if trainer.state.global_step != cfg.max_steps:
        raise ValueError("training did not reach the requested step")
    metrics = trainer.evaluate()
    loss = float(metrics["eval_loss"])
    if not math.isfinite(loss) or loss < 0:
        raise ValueError("invalid held-out evaluation")
    save_trained_model(cfg, trainer, model)
    tokenizer.save_pretrained(cfg.output_dir)
    publish(receipt, {"version": 1, "phase": "train", "step": cfg.max_steps, "evalLoss": loss, "reloaded": False})


def reload(config_path: Path, receipt: Path) -> None:
    import torch
    from peft import PeftModel
    from transformers import AutoModelForCausalLM, AutoTokenizer, BitsAndBytesConfig

    cfg: dict[str, object] = json.loads(config_path.read_text(encoding="utf-8"))
    quantization = None
    if cfg["load_in_4bit"]:
        quantization = BitsAndBytesConfig(
            load_in_4bit=True,
            bnb_4bit_quant_type="nf4",
            bnb_4bit_use_double_quant=True,
            bnb_4bit_compute_dtype=torch.bfloat16,
        )
    base = AutoModelForCausalLM.from_pretrained(
        cfg["base_model"],
        local_files_only=True,
        trust_remote_code=False,
        use_safetensors=True,
        torch_dtype=torch.bfloat16,
        attn_implementation="sdpa",
        device_map={"": 0},
        quantization_config=quantization,
    )
    model = PeftModel.from_pretrained(base, cfg["output_dir"], is_trainable=False, local_files_only=True)
    tokenizer = AutoTokenizer.from_pretrained(cfg["output_dir"], local_files_only=True, trust_remote_code=False)
    model.eval()
    tokens = tokenizer("The", return_tensors="pt").to(model.device)
    with torch.inference_mode():
        logits = model(**tokens).logits
        if not torch.isfinite(logits).all().item():
            raise ValueError("nonfinite reload output")
        generated = model.generate(**tokens, max_new_tokens=8, do_sample=False)
        if generated.shape[1] <= tokens["input_ids"].shape[1]:
            raise ValueError("reload produced no continuation")
    publish(receipt, {"version": 1, "phase": "reload", "step": cfg["max_steps"], "evalLoss": 0.0, "reloaded": True})


def main(args: list[str]) -> None:
    if len(args) != 4 or args[0] not in {"train", "reload"}:
        raise ValueError("invalid worker invocation")
    os.umask(0o077)
    site = Path(sys.executable).parent.parent / "lib" / f"python{sys.version_info.major}.{sys.version_info.minor}" / "site-packages"
    sys.path.insert(0, str(site))
    verify_environment()
    config, receipt, progress = (Path(value) for value in args[1:])
    if args[0] == "train":
        train(config, receipt, progress)
    else:
        reload(config, receipt)


if __name__ == "__main__":
    main(sys.argv[1:])
