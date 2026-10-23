# LLM training qualification

#453 stays open until the real GPU evidence below passes. Repository tests,
dependency resolution, discovery and import preparation do not approve a runtime.
No training preset is enabled. Checkpoint/resume is #454; Training UI is #456.
Keep host identities, paths, datasets, generated text and raw evidence private.

## Environment and backend decision

The [dependency input](../training/axolotl/requirements.in) is an explicit subset
for the worker's local SFT/LoRA path, with Torch 2.14.0 and its build tools.
It omits Gradio, Trackio, s3fs, gcsfs, adlfs, ocifs, lm_eval and openenv-core,
and overrides Axolotl's python-dotenv pin. The lock requires fsspec >=2026.6.0,
Pillow >=13 and python-dotenv >=1.2.2. These are nine deliberate deviations from
Axolotl's declared requirements: eight absent distributions and one version
override. They are checked exactly; additional or changed deviations fail CI.
The worker rejects the excluded distributions. Axolotl source and its installed
dependency metadata remain unchanged; no fork is used.

The CPU smoke test exercises the worker's actual setup, train, evaluate and save
sequence with a tiny random model, two LoRA optimizer steps and a separate-process
reload. It runs offline after installation and requires finite loss and logits.
This verifies the subset's code path on dependency updates, not GPU suitability.
The accompanying hash-locked requirements resolve for Linux x86_64 and CPython
3.12.14. They are a **package dependency candidate**, not a complete approved
environment. The [runtime check](#runtime-check) tests the interpreter, the
standard library, the installed wheels, the native libraries and the Torch and
CUDA compatibility with the GPU. It does not record provenance or run GPU kernels.
Source distributions need controlled builds. A source hash does not pin the
resulting wheel or its build environment. Do not install these into inference environments.

For a dedicated test environment, install the lock before the unchanged source,
then run both checks with that environment's interpreter:

```sh
uv pip sync --python "${PYTHON:?}" --require-hashes training/axolotl/requirements.txt
uv pip install --python "$PYTHON" --no-deps --no-build-isolation \
  'axolotl @ git+https://github.com/axolotl-ai-cloud/axolotl.git@1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1'
"$PYTHON" training/axolotl/check_environment.py
sudo unshare --net -- timeout 120 "$PYTHON" training/axolotl/smoke.py
```

Use an absolute `PYTHON` path. Installation requires access to the pinned artifacts;
the smoke runs in a network namespace with a two-minute limit, as in CI.
Ordinary `pip check` reports the nine intentional deviations.
Do not ignore arbitrary failures or edit installed metadata: use the exact
deviation checker. The CPU reload tests adapter compatibility; the production
worker's CUDA reload and bounded generation still need real-GPU evidence.

Regenerate only as an explicit dependency update, using uv 0.12.23:

```sh
uv pip compile training/axolotl/requirements.in \
  --python-version 3.12.14 --python-platform x86_64-unknown-linux-gnu \
  --generate-hashes --no-annotate --no-header \
  --output-file training/axolotl/requirements.txt
```

Before installation is approved, build/download a private wheelhouse, record its
hashes and the exact build-tool environment, and test installation offline. Install
Axolotl noneditable with `--no-deps` from commit `1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`
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

### Runtime check

`check_environment.py` prints one bounded JSON report. The `status` field is
`accepted` or `rejected`. The exit status is 1 when the report is `rejected`.
Use `--target` to select the target:

| Target | Rule |
| --- | --- |
| `cpu-smoke` (default) | GPU-only problems have the status `warn` and do not reject the CPU smoke |
| `smoke-lora` or `representative-qlora` | Each problem has the status `fail` and rejects the report |

| Check | What it tests |
| --- | --- |
| `python` | The interpreter is CPython 3.12, the lock target |
| `runtime-layout` | GPU only: a self-contained root with regular `bin/python3`, site-packages and standard library, as the runner needs |
| `standard-library` | Native standard-library modules load from the standard library |
| `dependencies` | The exact deviation contract, the security floors and the worker source guard |
| `locked-wheels` | Installed distributions match the names and versions in the lock, and each wheel tag fits this interpreter |
| `wheel-records` | Each installed file matches the hash and size in its wheel `RECORD` |
| `native-libraries` | Each `DT_NEEDED` library of Torch and of the bitsandbytes library for the Torch CUDA build resolves |
| `driver-libraries` | GPU only: the NVIDIA driver gives `libcuda.so.1` |
| `accelerator` | GPU only: the Torch CUDA build, the driver, the GPU architecture, BF16 and bitsandbytes for QLoRA |

The native check reads ELF headers and does not load the libraries. It follows
RUNPATH, the installed wheels, the loader cache and the default directories. It
ignores `LD_LIBRARY_PATH` because the runner does not set it. Optional wheel
plugins that Torch does not need are not part of the check.

The accelerator check needs a CUDA build of Torch and a visible GPU. Each GPU
needs SASS for its compute capability, or PTX for an equal or lower capability.
PTX needs a driver for the Torch CUDA version or newer. The presets need native
BF16, which starts at compute capability 8.0. When Torch has no usable GPU, the
report shows `"accelerator": "cpu"`.

Each problem tells what is wrong and what to change. The worker runs the same
accelerator check before it imports Axolotl. Thus a preset never falls back to
CPU training. The report contains no dataset content, credentials or host paths.
Run the check only in the dedicated training runtime. Activation and inference
paths do not run it.

## Immutable asset candidates

[training/assets.json](../training/assets.json) records two real source revisions:

| Preset | Model and tokenizer snapshot | Parameters |
| --- | --- | --- |
| `smoke-lora` | [TinyLlama 1.1B](https://huggingface.co/TinyLlama/TinyLlama-1.1B-intermediate-step-1431k-3T/tree/59f6f375b26bde864a6ca194a9a3044570490064) | LoRA 8/16, sequence 128, batch 1, accumulation 1, 2 steps |
| `representative-qlora` | [Llama 3.1 8B, NousResearch distribution](https://huggingface.co/NousResearch/Meta-Llama-3.1-8B/tree/1f47e50cdbe801ad8a5174156ec3a0655108fb9f) | QLoRA 16/32, sequence 2048, batch 1, accumulation 4, 100 steps |

Both presets use learning rate 0.0002 and seed 42. These are training weights,
not GGUF. The source revisions are pinned; no memory-fit or quality claim follows.
`assets.json` is the reference for qualification snapshots, not an input to
materialization. Presets fix training parameters; materialization accepts an
operator-supplied full revision and hashes the local inventory. Compare both
input revisions with this table when qualifying these reference candidates.
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
"${PYTHON:?}" training/axolotl/check_environment.py \
  --target "${PRESET:?}" > "$EVIDENCE/runtime-check.json"
```

Use the trainer GPU and the preset for `PRESET`. A `rejected` report blocks that
preset. Fix each `fail` problem in the report and run the check again.

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
