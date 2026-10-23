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
standard library, the installed versions, the native libraries and the Torch and
CUDA compatibility with the GPU. It does not record provenance or run GPU kernels.

Source distributions need controlled builds. A source hash does not pin the
resulting wheel or its build environment. Do not install these into inference environments.

For a dedicated test environment, install the lock before the unchanged source,
then run both checks with that environment's interpreter:

```sh
uv pip sync --python "${PYTHON:?}" --require-hashes training/axolotl/requirements.txt
uv pip install --python "$PYTHON" --no-deps --no-build-isolation \
  'axolotl @ git+https://github.com/axolotl-ai-cloud/axolotl.git@1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1'
"$PYTHON" -I -S -B training/axolotl/check_environment.py
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

Run `"$PYTHON" -I -S -B training/axolotl/check_environment.py` with the runtime's
own interpreter, as the runner does. The check refuses to run without `-I` and
`-S`, and it writes no bytecode even without `-B`. It adds the runtime
site-packages after the standard library, so `.pth` files never run. It prints
one bounded JSON report, version 1.

The exit status is 0 for `accepted` and 1 for `rejected`. The exit status 2 has
two causes. An internal error prints the same JSON keys with the status
`rejected`, empty `checks` and an `error` text. A usage error prints argparse
usage text on standard error and no JSON. The `--target` option selects the target:

| Target | Rule |
| --- | --- |
| `cpu-smoke` (default) | Preset-only problems have the status `warn` and do not reject the CPU smoke |
| `smoke-lora` or `representative-qlora` | Each problem has the status `fail` and rejects the report |

For a preset, give `--gpu-uuid` with the trainer GPU UUID, in the same format
that the supervisor accepts. The check then sets `CUDA_VISIBLE_DEVICES` to that
GPU for its child process, as the runner does. Without it, a preset target fails
the `accelerator` check.

| Report field | Value |
| --- | --- |
| `version`, `target`, `status` | `1`, the selected target (`null` when the exit status 2 comes before target selection), and `accepted` or `rejected` |
| `python`, `lockTarget` | The interpreter version, and `CPython 3.12.14`, the interpreter of the lock |
| `workerSHA256`, `lockSHA256` | SHA-256 of the embedded worker and of the lock |
| `accelerator` | `cuda`, `cpu`, or `null` when the probe or Torch gives no facts |
| `torch`, `cuda`, `driverCuda`, `archList`, `devices` | The Torch facts, or `null` when unknown |
| `checks` | Each entry has `name`, `status` (`pass`, `warn` or `fail`), at most 20 `problems` and an `omitted` count |
| `error` | `null`, or the internal error text for the exit status 2 |

| Check | What it tests |
| --- | --- |
| `python` | The runner rule: Python 3.12 or newer |
| `runtime-layout` | Preset only: not a virtual environment, and a root with regular `bin/python3`, site-packages and standard library |
| `standard-library` | Native standard-library modules load, and site-packages has no module with a standard-library name and no `sitecustomize` or `usercustomize` |
| `startup-files` | Preset only: the `RECORD` of a locked wheel lists each `.pth` file in site-packages |
| `dependencies` | The exact deviation contract, the security floors and the worker source guard |
| `locked-versions` | The installed names and versions match the lock, and each wheel tag fits this interpreter |
| `native-libraries` | Torch loads. For QLoRA, the bitsandbytes CUDA library and the bitsandbytes package load |
| `driver-libraries` | Preset only: `libcuda.so.1` from the NVIDIA driver loads |
| `accelerator` | Preset only: the Torch CUDA build, the driver, the GPU architecture, BF16 and bitsandbytes for QLoRA |

The standard-library names come from `sys.stdlib_module_names` and from the
private names, such as `_sysconfigdata_*`, in the standard-library directories.
The runner and the checker use `-S`, so `.pth` files never run.

The native, driver and accelerator checks run in a child process with `-I -S -B`.
The child gets the runner environment and puts the runtime site-packages first,
as the worker does. The runner drops all other variables, such as
`LD_LIBRARY_PATH`, `LD_PRELOAD` and `BNB_CUDA_VERSION`. Thus the child does not
get them. The check stops a child that runs longer than 300 seconds.

`uv pip sync --require-hashes` checks the integrity of the installation. The runtime
check finds version drift and wheels for another interpreter or platform. It does
not find changed or damaged files. Every target, also `cpu-smoke`, rejects
distributions that are not in the lock. Thus use `uv pip sync`, not
`uv pip install`, for the lock.

The lock installs xformers, which links `libcudart.so.12`. The lock installs no
`libcudart.so.12`, because Torch uses CUDA 13.0. The supported presets use SDPA
attention, so the check does not load xformers.

The accelerator check needs a CUDA build of Torch and a visible GPU. The driver
must support at least the CUDA major version of the Torch build.

SASS for a target runs when the target has the same major and an equal or lower
minor than the GPU.
PTX runs on a GPU with an equal or higher capability. PTX needs a driver for the
Torch CUDA version or newer. An `a` target, such as `sm_90a`, runs only on the exact
capability. An `f` target, such as `sm_100f`, runs only on the same major. The
presets need native BF16, which starts at compute capability 8.0.

When Torch has no usable GPU, the report shows `"accelerator": "cpu"`. Each
problem tells what is wrong and what to change. The report contains no dataset
content, credentials or host paths.

Before training starts, each worker phase runs the same accelerator check. Thus a
preset never falls back to CPU training. Run the check only in the dedicated
training runtime. Activation and inference paths do not run it.

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
"${PYTHON:?}" -I -S -B training/axolotl/check_environment.py --target "${PRESET:?}" \
  --gpu-uuid "${GPU_UUID:?}" > "$EVIDENCE/runtime-check.json"
```

Set `PRESET` to the preset and `GPU_UUID` to the UUID of the trainer GPU. A
`rejected` report blocks that preset. Fix each `fail` problem and run the check again.

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
