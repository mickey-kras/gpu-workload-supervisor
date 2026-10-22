# LLM training qualification

#453 stays open until the real GPU evidence below passes. Repository tests,
dependency resolution, discovery and import preparation do not approve a runtime.
No training preset is enabled. Checkpoint/resume is #454; Training UI is #456.
Keep host identities, paths, datasets, generated text and raw evidence private.

## Environment and backend decision

**Installation blocked:** the dependency candidate fails the repository security
gates. The pinned Trackio 0.40.0 requires Pillow <13, while the scanner requires
Pillow >=13.0.0; pinned s3fs/gcsfs 2025.10.0 require fsspec 2025.10.0, while the
scanner requires fsspec >=2026.6.0. FastMCP >=3.2.0 requires python-dotenv >=1.1.0,
but Axolotl pins python-dotenv 1.0.1. The latest inspected Axolotl commit
`2f98a743e8ee61bb6bb6f093dec296c5824a409c` retains these constraints.
Do not install this lock or bypass dependency metadata. A compatible, reviewed
upstream revision (or an explicitly approved source fork) must replace this
candidate before installation and GPU qualification. The rejected lock is retained
for review; it is not an installation recommendation.

The candidate [dependency input](../training/axolotl/requirements.in) is derived
from Axolotl's pinned `pyproject.toml`, with Torch 2.14.0 and its build tools.
The accompanying hash-locked requirements resolve for Linux x86_64 and CPython
3.12.14. They are a **package dependency candidate**, not a complete approved
environment: interpreter/stdlib, built wheels, OS libraries, CUDA driver/kernel
compatibility and GPU architecture still require deployment evidence. In particular,
source distributions require controlled builds; a source hash does not pin the
resulting wheel or its build environment. Do not install these into inference environments.

Regenerate only as an explicit dependency update, using uv 0.12.23:

```sh
uv pip compile training/axolotl/requirements.in \
  --python-version 3.12.14 --python-platform x86_64-unknown-linux-gnu \
  --generate-hashes --no-annotate --no-header \
  --output-file training/axolotl/requirements.txt
```

Before installation is approved, build/download a private wheelhouse, record its
hashes and the exact build-tool environment, and test installation offline. Install
Axolotl noneditable from commit `1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`
(version 0.21.0); preserve its real VCS `direct_url.json`. Do not fabricate that
metadata to satisfy the worker. The runner needs regular `bin/python3` and
`lib/pythonX.Y/site-packages` files with complete inventories; a usual symlinked
virtualenv is not this runtime layout. Record the approved interpreter, stdlib and
external shared-library closure separately, including hashes and provenance.

Source review on 2026-10-22 establishes these constraints, not a performance winner:

| Candidate source | Verified integration constraint | Remaining evidence |
| --- | --- | --- |
| [Axolotl 1f1eb05](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/pyproject.toml) | Python >=3.12; Transformers 5.20.1, Datasets 5.0.1, PEFT 0.21.2, TRL 1.14.1; existing supervised worker targets this source | Complete installed closure, both recipes, resource measurements |
| [TRL 1.14.1 fd74bbc](https://github.com/huggingface/trl/blob/fd74bbc7b5f852a70d4cc94377e0a8f94392fda1/pyproject.toml) with PEFT | Python >=3.10; Datasets >=4.7.0 and Transformers >=4.56.2; independent reference adapter still needs qualification | Equivalent tokenization, loss, optimizer steps, export and fresh reload |
| [Unsloth 356721b](https://github.com/unslothai/unsloth/blob/356721b31ebef3c1c0fcb8d53572c5be3d6eba89/pyproject.toml) | Its training extra caps Transformers at 5.17.0 and Datasets below 5.0.0; cannot share this Axolotl environment | Separate resolved closure, equivalent recipe and measured comparison |

Run each backend in its own pinned environment against the same asset revisions,
dataset bytes/splits, seed, LoRA targets, rank/alpha/dropout, BF16, sequence length,
batch/accumulation, optimizer, learning rate and number of optimizer steps. Disable
packing, reporting and uploads. Record tokenization differences explicitly; matching
row counts alone does not make the workloads equivalent. Do not bypass dependency
constraints to force a shared environment. The production runner currently supports
only the Axolotl candidate; a TRL/Unsloth experiment is not an accepted trainer.

## Immutable asset candidates

[training/assets.json](../training/assets.json) records two real source revisions:

| Preset | Model and tokenizer snapshot | Parameters |
| --- | --- | --- |
| `smoke-lora` | [TinyLlama 1.1B](https://huggingface.co/TinyLlama/TinyLlama-1.1B-intermediate-step-1431k-3T/tree/59f6f375b26bde864a6ca194a9a3044570490064) | LoRA 8/16, sequence 128, batch 1, accumulation 1, 2 steps |
| `representative-qlora` | [Llama 3.1 8B, NousResearch distribution](https://huggingface.co/NousResearch/Meta-Llama-3.1-8B/tree/1f47e50cdbe801ad8a5174156ec3a0655108fb9f) | QLoRA 16/32, sequence 2048, batch 1, accumulation 4, 100 steps |

Both presets use learning rate 0.0002 and seed 42. These are training weights,
not GGUF. The source revisions are pinned; no memory-fit or quality claim follows.
Use the model's applicable license. Keep downloaded model and tokenizer in
separate, new directories. With an already installed download client, use its
[snapshot_download](https://huggingface.co/docs/huggingface_hub/guides/download)
API with the exact `revision` above and `local_dir`; allow only model
`config.json`, `generation_config.json`, `model*.safetensors` and shard-index JSON,
and tokenizer `tokenizer.json`, `tokenizer.model`, `tokenizer_config.json`,
`special_tokens_map.json` and `added_tokens.json`. Exclude pickle weights and code.
Remove only the downloader's `.cache/huggingface` metadata in those new download
directories before inventorying; otherwise the exact asset inventory rejects it.
Download/install remains an explicit deployment step outside activation.

## Materialize and inspect

Write a private input JSON object with exactly these fields:

| Field | Value |
| --- | --- |
| `version`, `preset` | `1`; `smoke-lora` or `representative-qlora` |
| `model`, `tokenizer` | Each has `root` (absolute local directory) and `revision` (the full snapshot SHA) |
| `runtimeRoot` | Absolute dedicated runtime directory |
| `train`, `heldOut` | Absolute, distinct local JSONL paths using the candidate text schema |
| `outputDirectory` | New, empty, private absolute directory |
| `budget` | All fields from [candidate resource estimates](LLM-TRAINING-CANDIDATE.md#resource-estimates); explicit operator estimates, never zeros or inferred from model size |

Use absolute paths for the shell variables below. New recipe/plan files must not
already exist; their parent directories must be private. Use different output
directories, recipes and job IDs for every attempt.

```sh
gpu-training-candidate -version
gpu-training-runner -version
gpu-training-candidate -materialize "${INPUTS:?}" -output "${RECIPE:?}"
gpu-training-candidate -recipe "$RECIPE" -output "${PLAN:?}"
sha256sum "$RECIPE" "$PLAN"
```

Materialization records observed bytes and runs existing candidate validation;
it does not establish where those bytes originated. The original input and recipe
remain private. The [trainer binding](TRAINING.md) also needs the exact unit,
executable and recipe hashes, physical GPU UUID, cgroup, distinct job/terminal
paths and a **measured** `requiredMiB`. Read-only trainer inspection can verify
that binding; it cannot invent the absent measurement or approve the environment.

## Private host inputs and evidence

Collect these before choosing a deployment lock. These commands read system
state; redirect their output to a private directory, never a public issue or CI log:

```sh
umask 077
mkdir -m 700 "${EVIDENCE:?}"
uname -r > "$EVIDENCE/kernel.txt"
uname -m > "$EVIDENCE/architecture.txt"
systemctl --version > "$EVIDENCE/systemd.txt"
nvidia-smi --query-gpu=uuid,name,driver_version,memory.total \
  --format=csv,noheader > "$EVIDENCE/gpu.csv"
free -b > "$EVIDENCE/ram.txt"
df -B1 -- "${OUTPUT_PARENT:?}" > "$EVIDENCE/storage.txt"
stat -fc %T /sys/fs/cgroup > "$EVIDENCE/cgroup-filesystem.txt"
```

Also record: release/checksum, approved Python path/version and stdlib inventory;
all installed distribution metadata and wheel/source hashes; external ELF loader
and library identities; CUDA runtime, Torch build, supported GPU architecture and
driver compatibility; the reviewed egress containment; model/tokenizer source and
inventories; dataset digests/counts; recipe/plan; unit bytes and loaded properties;
existing inference catalog/revision for handoff verification. Do not collect raw
environment dumps or lifecycle completion tokens into reports.

After the environment and measured trainer binding are ready, run through the
supervisor, never by starting the worker or trainer unit directly. Use the installed
operator's existing state/runtime flags before each command where required:

```sh
gpu-mode -target "${TRAINER_PROFILE:?}" -training-job "${JOB_ID:?}" training-start
gpu-mode -training-job "$JOB_ID" training-status
gpu-mode -training-job "$JOB_ID" training-reconcile
gpu-mode -training-job "$JOB_ID" training-status
```

Reconcile only after the trainer exits; missing/invalid evidence leaves admission
pending. For interruption or uncertain launch, explicitly run
`gpu-mode -training-job "$JOB_ID" training-recover`; it stops and verifies release,
never resumes or retries training. Keep the output private for every command.

For each backend and size, retain the following evidence privately:

| Requirement | Required observation |
| --- | --- |
| Real training | Finite forward/backward execution and exactly the requested optimizer step count |
| Quality boundary | Finite nonnegative held-out loss; preserved split identity; no generated text in supervisor logs |
| Reload | Separate process loads the exact base, adapter/config and saved tokenizer; finite forward output and bounded continuation |
| Capacity | Peak process/cgroup RAM and GPU VRAM; free disk throughout; retained checkpoints plus one in-progress save, cache and export |
| Completion | Valid fenced completion accepted by reconcile; recursive cgroup empty and GPU compute processes released |
| Failure | Controlled interruption/failure never publishes success; recovery releases admission only after verified cleanup |
| Handoff | Existing inference profile starts through the supervisor after release and serves the expected model |
| Serving import | Explicit prepared artifact matches its manifest; separately selected serving runtime/version loads the exact base+adapter correctly |

Sample `nvidia-smi` memory usage throughout the job and retain cgroup `memory.peak`
before cleanup. Record sample interval: sampled GPU usage is a lower bound, not
an exact peak. Capture disk usage during saves, not just after pruning. Add safety
headroom to observed values before setting capacity. Failures, unmeasured peaks,
missing evidence or unsupported combinations keep qualification incomplete.

The [import preparation tool](LLM-TRAINING-IMPORT.md) validates artifact identity.
It does not merge/convert weights, modify a serving model or activate a runtime.
Keep #453 open until a reviewed deployment record covers every real-GPU gate.
