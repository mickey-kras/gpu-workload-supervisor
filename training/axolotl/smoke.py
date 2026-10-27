"""Tiny offline CPU SFT/LoRA regression, not real-GPU qualification."""
import importlib.util
import json
import math
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile


def deny_network(event, args):
    if event == "socket.getaddrinfo" or (event in {"socket.connect", "socket.sendto"} and args[0].family != socket.AF_UNIX):
        raise RuntimeError("network access is forbidden during the runtime smoke")


sys.addaudithook(deny_network)
for key in ("HF_HUB_OFFLINE", "HF_DATASETS_OFFLINE", "TRANSFORMERS_OFFLINE",
            "HF_HUB_DISABLE_TELEMETRY", "AXOLOTL_DO_NOT_TRACK", "DO_NOT_TRACK"):
    os.environ[key] = "1"
os.environ.update(WANDB_MODE="disabled", TOKENIZERS_PARALLELISM="false", CUDA_VISIBLE_DEVICES="", OMP_NUM_THREADS="1", AXOLOTL_DATASET_NUM_PROC="0")

import torch

torch.set_num_threads(1)


def worker_module():
    source = Path(__file__).resolve().parents[2] / "internal/llmtraining/worker.py"
    spec = importlib.util.spec_from_file_location("training_worker", source)
    worker = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(worker)
    worker.verify_environment()
    return worker


def reload_cpu(root):
    # Production reload intentionally requires CUDA/BF16; this fresh process tests
    # the same local safetensors/PEFT export contract using CPU float32.
    from peft import PeftModel
    from transformers import AutoModelForCausalLM, AutoTokenizer
    worker_module()
    model = AutoModelForCausalLM.from_pretrained(root / "base", local_files_only=True,
        trust_remote_code=False, use_safetensors=True, torch_dtype=torch.float32)
    model = PeftModel.from_pretrained(model, root / "adapter", local_files_only=True, is_trainable=False)
    tokenizer = AutoTokenizer.from_pretrained(root / "adapter", local_files_only=True, trust_remote_code=False)
    model.eval()
    inputs = tokenizer("The small model learns", return_tensors="pt")
    with torch.inference_mode():
        if not torch.isfinite(model(**inputs).logits).all().item():
            raise ValueError("nonfinite reloaded logits")
    assert_no_optional_imports()


def assert_no_optional_imports():
    forbidden = set(worker_module().OMITTED_INTEGRATIONS.values()) | {"dotenv"}
    imported = {name.split(".")[0] for name in sys.modules}
    if forbidden & imported:
        raise ValueError("unexpected integration imports: " + repr(sorted(forbidden & imported)))


def train_cpu(root):
    from tokenizers import Tokenizer
    from tokenizers.models import WordLevel
    from tokenizers.pre_tokenizers import Whitespace
    from transformers import LlamaConfig, LlamaForCausalLM, PreTrainedTokenizerFast
    torch.manual_seed(42)
    vocab = {word: i for i, word in enumerate("<pad> <unk> <bos> <eos> The small model learns a useful lesson today . held out example".split())}
    backend = Tokenizer(WordLevel(vocab, unk_token="<unk>"))
    backend.pre_tokenizer = Whitespace()
    tokenizer = PreTrainedTokenizerFast(tokenizer_object=backend, pad_token="<pad>",
        unk_token="<unk>", bos_token="<bos>", eos_token="<eos>", model_max_length=32)
    tokenizer.save_pretrained(root / "base")
    model = LlamaForCausalLM(LlamaConfig(vocab_size=len(vocab), hidden_size=32, intermediate_size=64,
        num_hidden_layers=1, num_attention_heads=2, num_key_value_heads=2, max_position_embeddings=64,
        bos_token_id=2, eos_token_id=3, pad_token_id=0))
    model.save_pretrained(root / "base", safe_serialization=True)
    for name, text, count in (("train", "The small model learns a useful lesson today .", 8),
                              ("heldout", "The held out example .", 2)):
        (root / (name + ".jsonl")).write_text("".join(json.dumps({"text": text}) + "\n" for _ in range(count)))
    def dataset(name):
        return {"path": str(root / (name + ".jsonl")), "ds_type": "json", "type": "completion", "field": "text", "split": "train", "trust_remote_code": False}
    config = {
        "base_model": str(root / "base"), "tokenizer_config": str(root / "base"),
        "trust_remote_code": False, "datasets": [dataset("train")], "test_datasets": [dataset("heldout")],
        "dataset_prepared_path": str(root / "cache"), "output_dir": str(root / "adapter"),
        "adapter": "lora", "load_in_4bit": False, "load_in_8bit": False, "bf16": False, "fp16": False,
        "lora_r": 2, "lora_alpha": 4, "lora_dropout": 0.0, "lora_target_modules": ["q_proj", "v_proj"],
        "sequence_len": 32, "micro_batch_size": 1, "gradient_accumulation_steps": 1,
        "max_steps": 2, "learning_rate": 0.001, "seed": 42, "train_on_inputs": True,
        "sample_packing": False, "gradient_checkpointing": False, "optimizer": "adamw_torch",
        "lr_scheduler": "constant", "attn_implementation": "sdpa", "eval_strategy": "steps", "eval_steps": 2,
        "save_strategy": "no", "plugins": [], "use_wandb": False, "use_mlflow": False,
        "use_tensorboard": False, "use_comet": False, "use_trackio": False, "use_otel_metrics": False,
        "wandb_mode": "disabled", "dataset_num_proc": 0, "dataloader_num_workers": 0,
        "use_cpu": True,
    }
    config_path = root / "config.json"
    config_path.write_text(json.dumps(config))
    # The CPU smoke has no GPU by design; the supervised runner always checks the GPU.
    worker_module().train(config_path, root / "receipt.json", root / "progress.json", accelerator=False)
    receipt = json.loads((root / "receipt.json").read_text())
    if receipt["step"] != 2 or not math.isfinite(receipt["evalLoss"]) or receipt["evalLoss"] < 0:
        raise ValueError("smoke failed to complete two steps with finite held-out loss")
    progress = json.loads((root / "progress.json").read_text())
    vram = progress.get("vramPeakBytes", "missing")
    if progress["step"] != 2 or not (vram is None or (isinstance(vram, int) and vram > 0)):
        raise ValueError("smoke progress has no final step or no valid VRAM peak field")
    assert_no_optional_imports()
    subprocess.run([sys.executable, __file__, "--reload", str(root)], check=True, timeout=45)
    print("CPU smoke passed: two LoRA steps, finite held-out loss and fresh-process adapter reload")


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--reload":
        reload_cpu(Path(sys.argv[2]))
    elif len(sys.argv) == 1:
        with tempfile.TemporaryDirectory(prefix="axolotl-cpu-") as directory:
            train_cpu(Path(directory))
    else:
        raise SystemExit("usage: smoke.py")
