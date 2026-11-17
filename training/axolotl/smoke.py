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

# Without a GPU, the Triton kernels of flash-linear-attention cannot run. Block the
# package so transformers uses its Torch path for the linear-attention layers.
sys.modules["fla"] = None
import torch

torch.set_num_threads(1)


CONFIG_NAME = "config.json"
# The Go tests in internal/llmtraining read the same golden files.
TESTDATA = Path(__file__).resolve().parents[2] / "internal/llmtraining/testdata"


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
    from transformers import (
        AutoModelForCausalLM,
        AutoModelForImageTextToText,
        AutoTokenizer,
    )
    worker = worker_module()
    # The text-only class names the layers model.layers.*, not the
    # model.language_model.layers.* of the image-text model that training used.
    # PEFT loads the adapter into it without an error; the worker check must not.
    text_only = AutoModelForCausalLM.from_pretrained(root / "base", local_files_only=True,
        trust_remote_code=False, use_safetensors=True, dtype=torch.float32)
    text_only = PeftModel.from_pretrained(text_only, root / "adapter", local_files_only=True, is_trainable=False)
    try:
        worker.require_matching_adapter(text_only, root / "adapter")
    except ValueError as error:
        if "does not match the base model" not in str(error):
            raise
    else:
        raise ValueError("the adapter check accepted the text-only class")
    del text_only
    model = AutoModelForImageTextToText.from_pretrained(root / "base", local_files_only=True,
        trust_remote_code=False, use_safetensors=True, dtype=torch.float32)
    model = PeftModel.from_pretrained(model, root / "adapter", local_files_only=True, is_trainable=False)
    worker.require_matching_adapter(model, root / "adapter")
    tokenizer = AutoTokenizer.from_pretrained(root / "adapter", local_files_only=True, trust_remote_code=False)
    model.eval()
    inputs = tokenizer("The small model learns", return_tensors="pt")
    with torch.inference_mode():
        if not torch.isfinite(model(**inputs).logits).all().item():
            raise ValueError("nonfinite reloaded logits")
    assert_no_optional_imports()


def split_processor(directory):
    # The pinned snapshots hold split processor files, which the Go candidate
    # check requires. save_pretrained writes one combined file, so split it.
    combined = directory / "processor_config.json"
    saved = json.loads(combined.read_text())
    if saved.get("processor_class") != "Qwen3VLProcessor" or not all(isinstance(saved.get(part), dict) for part in ("image_processor", "video_processor")):
        raise ValueError("the smoke processor does not match the candidate check")
    for name, part, kind in (("preprocessor_config.json", "image_processor", "image_processor_type"),
                             ("video_preprocessor_config.json", "video_processor", "video_processor_type")):
        config = dict(saved[part], processor_class=saved["processor_class"])
        if config.get(kind) is None or "auto_map" in config or config["processor_class"] != "Qwen3VLProcessor":
            raise ValueError("the smoke processor does not match the candidate check")
        (directory / name).write_text(json.dumps(config))
    combined.unlink()


def shape(value):
    if isinstance(value, dict):
        return {key: shape(item) for key, item in value.items()}
    if isinstance(value, list):
        return [shape(item) for item in value if isinstance(item, (dict, list))] or "list"
    return type(value).__name__


def assert_saved_processor(adapter):
    # The Go export check needs a complete processor. Training must save it.
    combined = adapter / "processor_config.json"
    if not combined.is_file():
        raise ValueError("training saved no processor_config.json with the adapter")
    saved = json.loads(combined.read_text())
    if "auto_map" in saved or saved.get("processor_class") != "Qwen3VLProcessor":
        raise ValueError("the saved processor is not Qwen3VLProcessor without auto_map")
    # The same class values that the Go export check (processor.go) requires.
    image, video = saved.get("image_processor"), saved.get("video_processor")
    if not isinstance(image, dict) or not isinstance(video, dict) or video.get("video_processor_type") != "Qwen3VLVideoProcessor" \
            or image.get("image_processor_type") not in ("Qwen2VLImageProcessorFast", "Qwen2VLImageProcessor"):
        raise ValueError("the saved processor does not name the Qwen image and video processor classes")
    # The Go tests use this golden file (savedProcessorFixturePath in
    # internal/llmtraining); it must have the shape of real output.
    golden = TESTDATA / "qwen3.8-processor/saved_processor_config.json"
    if shape(saved) != shape(json.loads(golden.read_text())):
        raise ValueError("the saved processor keys differ from the golden saved_processor_config.json")


def assert_saved_config(adapter):
    # Training saves the model config.json with the adapter, and the Go export
    # check reads it. The Go tests use the golden file smoke-output/saved_config.json
    # in internal/llmtraining/testdata; it must have the shape of real output.
    saved = json.loads((adapter / CONFIG_NAME).read_text())
    text = saved.get("text_config")
    if saved.get("model_type") != "qwen3_5" or saved.get("architectures") != ["Qwen3_5ForConditionalGeneration"] \
            or not isinstance(text, dict) or text.get("model_type") != "qwen3_5_text":
        raise ValueError("the saved model config.json does not name the qwen3_5 image-text architecture")
    golden = TESTDATA / "smoke-output/saved_config.json"
    if shape(saved) != shape(json.loads(golden.read_text())):
        raise ValueError("the saved model config.json keys differ from the golden smoke-output/saved_config.json")


def assert_qlora_quantization(root, config):
    # The Go export accepts only the quantization_config that QLoRA training
    # saves (qloraQuantizationConfig, golden qlora/quantization_config.json).
    # Build it with Axolotl's own loader step from a QLoRA variant of this
    # config, and serialize it as save_pretrained does. No GPU is needed.
    from types import SimpleNamespace

    from axolotl.cli.config import load_cfg
    from axolotl.loaders.model import ModelLoader
    from transformers import AutoConfig

    qlora = dict(config, adapter="qlora", load_in_4bit=True, bf16=True)
    path = root / "qlora-config.json"
    path.write_text(json.dumps(qlora))
    loader = SimpleNamespace(cfg=load_cfg(str(path)), model_config=SimpleNamespace(), model_kwargs={}, is_fsdp_enabled=False)
    ModelLoader._set_quantization_config(loader)
    model_config = AutoConfig.from_pretrained(root / "base", local_files_only=True, trust_remote_code=False)
    model_config.quantization_config = loader.model_kwargs["quantization_config"]
    saved = json.loads(model_config.to_json_string(use_diff=True))["quantization_config"]
    golden = TESTDATA / "qlora/quantization_config.json"
    if saved != json.loads(golden.read_text()):
        raise ValueError("Axolotl's QLoRA quantization_config differs from the golden qlora/quantization_config.json")


def assert_no_optional_imports():
    forbidden = set(worker_module().OMITTED_INTEGRATIONS.values()) | {"dotenv"}
    imported = {name.split(".")[0] for name in sys.modules}
    if forbidden & imported:
        raise ValueError("unexpected integration imports: " + repr(sorted(forbidden & imported)))


def train_cpu(root):
    from tokenizers import Tokenizer
    from tokenizers.models import WordLevel
    from tokenizers.pre_tokenizers import Whitespace
    from transformers import (
        PreTrainedTokenizerFast,
        Qwen2VLImageProcessor,
        Qwen3_5Config,
        Qwen3_5ForConditionalGeneration,
        Qwen3VLProcessor,
        Qwen3VLVideoProcessor,
    )
    torch.manual_seed(42)
    vocab = {word: i for i, word in enumerate("<pad> <unk> <bos> <eos> The small model learns a useful lesson today . held out example".split())}
    backend = Tokenizer(WordLevel(vocab, unk_token="<unk>"))
    backend.pre_tokenizer = Whitespace()
    tokenizer = PreTrainedTokenizerFast(tokenizer_object=backend, pad_token="<pad>",
        unk_token="<unk>", bos_token="<bos>", eos_token="<eos>", model_max_length=32)
    # Production keeps the tokenizer and processor in a separate snapshot
    # without config.json; the smoke uses the same layout.
    tokenizer.save_pretrained(root / "tokenizer")
    # A tiny model of the presets' image-text architecture: one linear-attention
    # and one full-attention layer, and a small vision tower.
    text_config = {"vocab_size": len(vocab), "hidden_size": 32, "intermediate_size": 64, "num_hidden_layers": 2,
        "num_attention_heads": 2, "num_key_value_heads": 1, "head_dim": 16,
        "layer_types": ["linear_attention", "full_attention"], "full_attention_interval": 2,
        "linear_conv_kernel_dim": 4, "linear_key_head_dim": 8, "linear_num_key_heads": 2,
        "linear_num_value_heads": 2, "linear_value_head_dim": 8, "max_position_embeddings": 64,
        "bos_token_id": 2, "eos_token_id": 3, "pad_token_id": 0,
        "rope_parameters": {"rope_type": "default", "rope_theta": 10000.0, "partial_rotary_factor": 0.25,
                            "mrope_section": [1, 1, 0], "mrope_interleaved": True}}
    vision = {"depth": 1, "hidden_size": 16, "intermediate_size": 32, "num_heads": 2, "out_hidden_size": 32,
        "patch_size": 4, "spatial_merge_size": 2, "temporal_patch_size": 2, "num_position_embeddings": 16}
    model = Qwen3_5ForConditionalGeneration(Qwen3_5Config(text_config=text_config, vision_config=vision, tie_word_embeddings=False))
    model.save_pretrained(root / "base", safe_serialization=True)
    if (root / "tokenizer" / CONFIG_NAME).exists():
        raise ValueError("the tokenizer snapshot must not contain config.json")
    Qwen3VLProcessor(image_processor=Qwen2VLImageProcessor(patch_size=4, merge_size=2, temporal_patch_size=2),
        video_processor=Qwen3VLVideoProcessor(patch_size=4, merge_size=2, temporal_patch_size=2),
        tokenizer=tokenizer).save_pretrained(root / "tokenizer")
    split_processor(root / "tokenizer")
    for name, text, count in (("train", "The small model learns a useful lesson today .", 8),
                              ("heldout", "The held out example .", 2)):
        (root / (name + ".jsonl")).write_text("".join(json.dumps({"text": text}) + "\n" for _ in range(count)))
    def dataset(name):
        return {"path": str(root / (name + ".jsonl")), "ds_type": "json", "type": "completion", "field": "text", "split": "train", "trust_remote_code": False}
    config = {
        "base_model": str(root / "base"), "tokenizer_config": str(root / "tokenizer"),
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
    config_path = root / CONFIG_NAME
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
    assert_saved_processor(root / "adapter")
    assert_saved_config(root / "adapter")
    assert_qlora_quantization(root, config)
    assert_no_optional_imports()
    # The CI step stops the whole smoke after 300 s; the reload takes seconds.
    subprocess.run([sys.executable, __file__, "--reload", str(root)], check=True, timeout=60)
    print("CPU smoke passed: two LoRA steps, finite held-out loss and fresh-process adapter reload")


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--reload":
        reload_cpu(Path(sys.argv[2]))
    elif len(sys.argv) == 1:
        with tempfile.TemporaryDirectory(prefix="axolotl-cpu-") as directory:
            train_cpu(Path(directory))
    else:
        raise SystemExit("usage: smoke.py")
