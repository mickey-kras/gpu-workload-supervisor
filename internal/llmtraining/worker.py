"""Pinned Axolotl experiment worker; invoked only by the supervised runner."""

from __future__ import annotations

import contextlib
import ctypes
import importlib.metadata
import importlib.util
import json
import math
import os
from pathlib import Path
import re
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


# Remedies for the runtime problems. The runner discards them; the runtime check reports them.
SYNC = "reinstall the training runtime from the lock with uv pip sync --require-hashes"
SOURCE = f"install axolotl noneditable with --no-deps from commit {SOURCE_REVISION}"

# The pinned Axolotl source declares Python >=3.12. The runner and the runtime
# check share this rule; the lock itself targets CPython 3.12.14.
MINIMUM_PYTHON = (3, 12)


def python_problem(version: tuple[int, ...]) -> str | None:
    if tuple(version[:2]) < MINIMUM_PYTHON:
        return "training runtime requires Python 3.12 or newer"
    return None


def runtime_site() -> Path:
    # The runner starts bin/python3 with -I -S; only the runtime's site-packages is added.
    version = f"python{sys.version_info.major}.{sys.version_info.minor}"
    return Path(sys.executable).parent.parent / "lib" / version / "site-packages"


# These optional integrations are deliberately absent from the offline SFT runtime.
# Check both metadata and importability: an unpackaged module is also unsafe here.
OMITTED_INTEGRATIONS = {
    "trackio": "trackio", "gradio": "gradio", "s3fs": "s3fs", "gcsfs": "gcsfs",
    "adlfs": "adlfs", "ocifs": "ocifs", "lm-eval": "lm_eval", "openenv-core": "openenv",
}


def verify_subset() -> None:
    installed = {
        re.sub(r"[-_.]+", "-", distribution.metadata["Name"]).lower()
        for distribution in importlib.metadata.distributions()
    }
    for distribution, module in OMITTED_INTEGRATIONS.items():
        if distribution in installed or importlib.util.find_spec(module) is not None:
            raise ValueError(f"unapproved optional runtime integration: {distribution}; {SYNC}")


def verify_environment() -> None:
    verify_subset()
    distribution = importlib.metadata.distribution("axolotl")
    direct_url = json.loads(distribution.read_text("direct_url.json") or "{}")
    if not isinstance(direct_url, dict):
        raise ValueError("runtime source is not pinned; " + SOURCE)
    vcs_info = direct_url.get("vcs_info")
    dir_info = direct_url.get("dir_info", {})
    if (
        not isinstance(vcs_info, dict)
        or vcs_info.get("commit_id") != SOURCE_REVISION
        or not isinstance(dir_info, dict)
        or dir_info.get("editable", False)
    ):
        raise ValueError("runtime source is not pinned; " + SOURCE)
    for point in importlib.metadata.entry_points():
        if point.group.startswith("axolotl.") and (
            point.group not in BUILTIN_ENTRY_POINTS
            or BUILTIN_ENTRY_POINTS[point.group].get(point.name) != point.value
            or point.dist is None
            or point.dist.metadata["Name"].lower() != "axolotl"
        ):
            raise ValueError("unapproved runtime entry point; remove the distribution that registers it")


# Torch wheels carry SASS for sm_XY targets and PTX for compute_XY targets. SASS
# runs when the target has the same major and an equal or lower minor than the
# GPU. PTX JIT-compiles for any equal or lower target capability, with a driver
# at least as new as the CUDA build. Architecture-specific "a" targets run only
# on the exact capability. Family "f" targets run only on the same major.
def kernel_support(capability: tuple[int, int], arch_list: list[str]) -> str:
    support = ""
    for arch in arch_list:
        match = re.fullmatch(r"(sm|compute)_(\d+)(\d)([af]?)", arch)
        if match is None:
            continue
        target = (int(match.group(2)), int(match.group(3)))
        if (match.group(4) == "a" and target != capability) or target > capability:
            continue
        if match.group(1) == "sm" and target[0] == capability[0]:
            return "sass"
        if match.group(1) == "compute" and (match.group(4) != "f" or target[0] == capability[0]):
            support = "ptx"
    return support


def version_pair(text: str) -> tuple[int, int]:
    major, minor = (text.split(".") + ["0"])[:2]
    return int(major), int(minor)


# Both presets train and reload in BF16 on one CUDA device; QLoRA also needs the
# bitsandbytes kernels. The driver must support at least the CUDA major version
# of the Torch build. Each problem states what is wrong and what to change.
def accelerator_problems(facts: dict[str, object], quantized: bool) -> list[str]:
    torch, build, driver = str(facts["torch"]), facts["cuda"], facts["driverCuda"]
    if facts["hip"] or not isinstance(build, str):
        return [f"Torch {torch} is not a CUDA build; install the CUDA torch wheel from the training lock"]
    if not facts["available"] or not facts["devices"]:
        return [(
            f"Torch CUDA {build} cannot use a GPU; "
            f"install an NVIDIA driver for CUDA {build} or newer and expose one GPU"
        )]
    problems = []
    if isinstance(driver, str) and version_pair(driver)[0] < version_pair(build)[0]:
        problems.append(
            f"the NVIDIA driver supports CUDA {driver}, older than the Torch CUDA {build} build; upgrade the driver"
        )
    problems.extend(gpu_problems(facts))
    if quantized and not facts["bitsandbytes"]:
        problems.append("QLoRA needs bitsandbytes; install bitsandbytes from the training lock")
    return problems


def gpu_problems(facts: dict[str, object]) -> list[str]:
    torch, build, driver = str(facts["torch"]), facts["cuda"], facts["driverCuda"]
    problems = []
    arch_list = [str(arch) for arch in facts["archList"]]
    for index, value in enumerate(facts["devices"]):
        capability = (int(value[0]), int(value[1]))
        name = f"GPU {index} (compute capability {capability[0]}.{capability[1]})"
        support = kernel_support(capability, arch_list)
        if not support:
            targets = " ".join(arch_list) or "an empty architecture list"
            problems.append(
                f"{name} has no kernels in Torch {torch} ({targets}); "
                f"install a Torch build with sm_{capability[0]}{capability[1]} or use a supported GPU"
            )
        elif support == "ptx" and (not isinstance(driver, str) or version_pair(driver) < version_pair(build)):
            problems.append(
                f"{name} needs PTX JIT compilation, which needs a driver for CUDA {build} or newer; upgrade the driver"
            )
        if capability < (8, 0):
            problems.append(
                f"{name} has no native BF16, which the training presets need; "
                "use a GPU with compute capability 8.0 or newer"
            )
    return problems


def driver_cuda() -> str | None:
    # Torch exposes no driver version, so ask the driver library that CUDA loads.
    try:
        version = ctypes.c_int()
        if ctypes.CDLL("libcuda.so.1").cuDriverGetVersion(ctypes.byref(version)) != 0:
            return None
    except (OSError, AttributeError):
        return None
    return f"{version.value // 1000}.{version.value % 1000 // 10}"


def torch_facts() -> dict[str, object]:
    import torch

    facts: dict[str, object] = {
        "torch": str(torch.__version__), "cuda": torch.version.cuda, "hip": getattr(torch.version, "hip", None),
        "available": False, "driverCuda": driver_cuda(), "archList": [], "devices": [],
        "bitsandbytes": importlib.util.find_spec("bitsandbytes") is not None,
    }
    if torch.version.cuda and torch.cuda.is_available():
        facts.update(
            available=True, archList=list(torch.cuda.get_arch_list()),
            devices=[list(torch.cuda.get_device_capability(index)) for index in range(torch.cuda.device_count())],
        )
    return facts


def verify_accelerator(quantized: object) -> None:
    # Fail before training starts: an unsupported GPU must never fall back to CPU.
    if not isinstance(quantized, bool):
        raise ValueError("training configuration load_in_4bit must be a boolean")
    problems = accelerator_problems(torch_facts(), quantized)
    if problems:
        raise ValueError("unsupported training accelerator: " + "; ".join(problems))


def publish(path: Path, value: dict[str, object]) -> None:
    temporary = path.with_suffix(".pending")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump(value, output, allow_nan=False)
            output.flush()
            os.fsync(output.fileno())
        temporary.replace(path)
    except BaseException:
        # A failed publish must not leave a file that blocks the next one.
        temporary.unlink(missing_ok=True)
        raise


def vram_peak(previous: int | None) -> int | None:
    # The Torch allocator peak of this process is a lower bound for the trainer.
    # Device memory in use is not: it counts every process on the GPU. Samples
    # can miss memory outside the allocator, such as the CUDA context.
    import torch

    if not torch.cuda.is_available():
        return previous
    reserved = int(torch.cuda.max_memory_reserved())
    # A process that never allocated on the GPU has no measured peak.
    if reserved <= 0:
        return previous
    return max(previous or 0, reserved)


def train(config_path: Path, receipt: Path, progress: Path, accelerator: bool = True) -> None:
    from axolotl.cli.config import load_cfg
    from axolotl.common.datasets import load_datasets
    from axolotl.train import (
        save_initial_configs,
        save_trained_model,
        setup_model_and_trainer,
    )
    from transformers import TrainerCallback, TrainerControl, TrainerState, TrainingArguments

    class Progress(TrainerCallback):
        vram: int | None = None

        def record(self, step: int) -> None:
            # A VRAM sample is diagnostic data, never a condition for completion.
            with contextlib.suppress(Exception):
                self.vram = vram_peak(self.vram)
            publish(progress, {"version": 1, "phase": "training", "step": step, "vramPeakBytes": self.vram})

        def on_step_end(
            self,
            args: TrainingArguments,
            state: TrainerState,
            control: TrainerControl,
            **kwargs: object,
        ) -> TrainerControl:
            self.record(state.global_step)
            return control

    cfg = load_cfg(str(config_path))
    # Axolotl's schema has no processor_config key, and its default is the model
    # directory. The processor files of the image-text model are in the
    # tokenizer snapshot, which has no model config.json.
    cfg.processor_config = getattr(cfg, "tokenizer_config", None)
    # Only the CPU smoke turns this off; the runner always checks the GPU.
    if accelerator:
        verify_accelerator(getattr(cfg, "load_in_4bit", None))
    dataset = load_datasets(cfg=cfg)
    trainer, model, tokenizer, adapter, processor = setup_model_and_trainer(cfg, dataset)
    save_initial_configs(cfg, tokenizer, model, adapter, processor)
    tracker = Progress()
    trainer.add_callback(tracker)
    trainer.train(resume_from_checkpoint=None)
    if trainer.state.global_step != cfg.max_steps:
        raise ValueError("training did not reach the requested step")
    metrics = trainer.evaluate()
    loss = float(metrics["eval_loss"])
    if not math.isfinite(loss) or loss < 0:
        raise ValueError("invalid held-out evaluation")
    with contextlib.suppress(Exception):
        tracker.record(trainer.state.global_step)
    save_trained_model(cfg, trainer, model)
    tokenizer.save_pretrained(cfg.output_dir)
    with contextlib.suppress(Exception):
        tracker.record(trainer.state.global_step)
    publish(receipt, {"version": 1, "phase": "train", "step": cfg.max_steps, "evalLoss": loss, "reloaded": False})


def require_matching_adapter(model, directory) -> None:
    """Reject an adapter whose saved keys differ from the modules of the base model."""
    from peft.utils import get_peft_model_state_dict, load_peft_weights

    saved = set(load_peft_weights(directory, device="cpu"))
    loaded = set(get_peft_model_state_dict(model))
    if not saved or saved != loaded:
        unexpected, missing = sorted(saved - loaded), sorted(loaded - saved)
        # Key names only, one sample each: no paths or tensor contents.
        raise ValueError(
            f"adapter does not match the base model: {len(saved)} saved and {len(loaded)} expected keys; "
            f"{len(unexpected)} unexpected {unexpected[:1]}, {len(missing)} missing {missing[:1]}"
        )


def reload(config_path: Path, receipt: Path, progress: Path) -> None:
    cfg: dict[str, object] = json.loads(config_path.read_text(encoding="utf-8"))
    verify_accelerator(cfg.get("load_in_4bit"))
    import torch
    from peft import PeftModel
    from transformers import (
        AutoModelForImageTextToText,
        AutoTokenizer,
        BitsAndBytesConfig,
    )

    quantization = None
    if cfg["load_in_4bit"]:
        quantization = BitsAndBytesConfig(
            load_in_4bit=True,
            bnb_4bit_quant_type="nf4",
            bnb_4bit_use_double_quant=True,
            bnb_4bit_compute_dtype=torch.bfloat16,
        )
    # Load the image-text class that training used and that vLLM serves, so the
    # adapter keys name the same modules.
    base = AutoModelForImageTextToText.from_pretrained(
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
    require_matching_adapter(model, cfg["output_dir"])
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
    with contextlib.suppress(Exception):
        publish(progress, {"version": 1, "phase": "reload", "step": cfg["max_steps"], "vramPeakBytes": vram_peak(None)})
    publish(receipt, {"version": 1, "phase": "reload", "step": cfg["max_steps"], "evalLoss": 0.0, "reloaded": True})


def final_sample(progress: Path, phase: str, error: BaseException) -> None:
    # After a CUDA out-of-memory error, the allocator still works and its peak is
    # the useful value, so the worker tries to write one last VRAM sample. Other
    # errors make no CUDA call: a broken device call could block the exit. This
    # never hides the original error. A killed worker writes none.
    with contextlib.suppress(Exception):
        torch = sys.modules.get("torch")
        if torch is None or not isinstance(error, torch.cuda.OutOfMemoryError):
            return
        previous = json.loads(progress.read_text(encoding="utf-8")) if progress.exists() else {}
        step = previous.get("step") if isinstance(previous, dict) else None
        last = previous.get("vramPeakBytes") if isinstance(previous, dict) else None
        vram = vram_peak(last if isinstance(last, int) else None)
        if vram is not None:
            publish(progress, {"version": 1, "phase": phase, "step": step if isinstance(step, int) else 0, "vramPeakBytes": vram})


def main(args: list[str]) -> None:
    if len(args) != 4 or args[0] not in {"train", "reload"}:
        raise ValueError("invalid worker invocation")
    problem = python_problem((sys.version_info.major, sys.version_info.minor))
    if problem is not None:
        raise ValueError(problem)
    os.umask(0o077)
    sys.path.insert(0, str(runtime_site()))
    verify_environment()
    config, receipt, progress = (Path(value) for value in args[1:])
    try:
        if args[0] == "train":
            train(config, receipt, progress)
        else:
            reload(config, receipt, progress)
    except BaseException as error:
        final_sample(progress, "training" if args[0] == "train" else "reload", error)
        raise


if __name__ == "__main__":
    main(sys.argv[1:])

