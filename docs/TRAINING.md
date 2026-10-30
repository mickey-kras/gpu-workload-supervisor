# Training

## LLM training candidate preparation

This is a preparation-only increment for #453. It does not launch training,
enable a catalog profile, approve a runtime, or make an output eligible for
serving import. This increment publishes no GPU memory or storage measurements.

The release archive and Debian package include `gpu-training-candidate`. Check
the release with `gpu-training-candidate -version`. Then run it with two
absolute paths:

```sh
gpu-training-candidate -recipe /private/candidate.json -output /private/new-plan.json
```

To create the recipe from local directories and explicit resource estimates, use
`gpu-training-candidate -materialize /private/inputs.json -output /private/new-recipe.json`.
The [qualification guide](TRAINING.md#materialize-and-inspect)
defines the strict input object and pins real smoke and representative asset
candidates. Materialization inventories observed bytes. It does not
authenticate their source or approve a runtime. It never downloads, installs or
executes training code.

Job-bound preparation accepts the supervisor's finite-job envelope instead of
`-recipe`:

```sh
gpu-training-candidate -job-file /private/training/job.json -output /private/new-plan.json
```

This mode requires the current systemd `INVOCATION_ID`. It checks the launch
identity and the SHA256 of the original recipe bytes, then repeats candidate
input and resource checks. The private plan carries the job identity and
invocation, but never its completion token. It is preparation evidence only: it
cannot acknowledge completion, approve the runtime or authorize GPU work. The
command checks the invocation value and envelope locally. This does not prove
that the invocation is live or the fence is still current.

The command reads local inputs and creates one new file with mode `0600`. It
refuses to replace an existing file. Keep its parent directory private. The
command writes the plan to a private temporary file in that directory.

It publishes the plan only after writing and closing the file successfully.
Publication requires hard-link support on the destination filesystem. A failed
write leaves no final plan to block retry. The preparation command has no
network access, runtime import, subprocess launch or dataset-content logging.

Errors omit input values and file contents.

The recipe-mode plan contains `status: "unqualified"`, input row counts,
operator-supplied resource estimates, a canonical recipe digest and a generated
`config` object. Job-mode plans nest these fields under `candidate`, including
`candidate.config` and `candidate.canonicalRecipeSHA256`. The outer
`recipeSHA256` identifies the original recipe bytes. The plan itself is **not**
an Axolotl configuration file. Its `config` object is JSON-subset YAML
targeting the inspected source revision below. A successful compile does not
show that Axolotl can load or train this model.

### Recipe version 1

Recipes are strict JSON, at most 8 MiB. The command rejects unknown keys,
duplicate keys, trailing values, remote paths and arbitrary trainer or
configuration overrides. The complete typed schema is
`internal/llmtraining/recipe.go`.

| Field | Required value |
| --- | --- |
| `version` | `1` |
| `model`, `tokenizer` | Separate objects with an exact lowercase 40-hex `revision` and an `inventory` |
| `runtime` | An `inventory` and relative `entrypoint` present in that inventory |
| Each `inventory` | Absolute `root` and `files` mapping every relative regular-file path to its lowercase SHA256 |
| `train`, `heldOut` | Separate objects with absolute `path` and lowercase `sha256` |
| `outputDirectory` | Existing empty directory, distinct from and outside all inputs |
| `adapter` | `lora` or `qlora` |
| `rank`, `alpha` | 1-64 and 1-128 |
| `sequenceLength` | 128-4096 |
| `microBatchSize`, `gradientAccumulation` | 1-4 and 1-64 |
| `maxSteps` | 1-10000 |
| `learningRate` | 0.000001-0.001 |
| `seed` | 0-2147483647 |
| `budget` | The complete estimates below |

Inventories reject missing, extra, linked or nonregular files. Each inventory
permits at most 65536 enumerated entries, including empty directories but
excluding the root, and declares at most 32768 files. Directory reads stop at
the remaining entry budget plus one overflow entry. The output directory check
reads one entry.

Inventory matching compares bytes only at preparation time. It does not prove
the bytes originate from the declared revision, approve code or installed entry
points, bind external libraries, or prevent later modification. Include
installed distribution metadata and entry-point declarations in the runtime
inventory.

The command never imports the runtime, even to discover its version or plugins.

The candidate supports only `model_type: "llama"`. It needs local
`config.json`, at least one `.safetensors` file, and a separate
`tokenizer_config.json`. Asset inventories allow only `.json`, `.safetensors`,
`.model` and `.txt` files. The command rejects configurations with `auto_map`
or model `quantization_config`.

GGUF, pickle weights, prequantized input and custom model code are outside this
candidate. File names and configuration checks do not validate tensor shapes,
tokenizer compatibility or actual model loading. Those remain qualification
steps.

Each dataset is UTF-8 JSONL containing only `{"text":"..."}` records. Limits
are 64 MiB per file, less than 64 KiB per line and 50000 records per split. The
command rejects empty text and exact decoded-text overlap between training and
held-out splits. This is not semantic deduplication.

Tokenization occurs later. Row counts are not token counts. Axolotl's
completion strategy can chunk a long record into multiple sequences. Keep
private dataset contents and resulting artifacts outside public repository
history.

### Resource estimates

Every byte estimate must be nonzero: `ramBytes`, `weightsBytes`, `cacheBytes`,
`checkpointBytes`, `exportBytes`, `reserveBytes`. `retainedCheckpoints` is 1-8.
The overflow-checked storage estimate is:

```
weights + cache + export + reserve + checkpoint * (retainedCheckpoints + 1)
```

The extra checkpoint accounts for an in-progress save. Preparation compares
this conservative total with available space on the output filesystem and
compares RAM with Linux free RAM, excluding reclaimable cache. It reserves
neither RAM nor disk and cannot predict training use. The total still includes
existing weights. Generated dataset cache and training output share the output
directory. These checks are estimates, not measured capacity or GPU
compatibility checks.

### Candidate configuration

The fixed configuration uses full-text completion, explicit held-out data, no
sample packing, BF16, SDPA, gradient checkpointing, AdamW, a constant learning
rate, and LoRA targets `q_proj`/`v_proj`. QLoRA enables Axolotl's NF4
conversion with double quantization. LoRA leaves 4-bit and 8-bit loading off.
The configuration requests evaluation and checkpoint saving at the final step.

Automatic resume is off. The inspected upstream default also evaluates before
training. Checkpoints request optimizer and training state
(`save_only_model: false`), which does not show checkpoint-stop or resume
correctness.

The generated configuration disables reporting integrations and Hub uploads.
This alone is not network containment. The experimental runner uses a sanitized
environment and disables telemetry with `AXOLOTL_DO_NOT_TRACK=1`. The complete
runtime still requires qualification. Axolotl can auto-load installed plugin or
model-support entry points even when `plugins` is empty. This tool does not
approve those plugins.

`canonicalRecipeSHA256` hashes the typed recipe re-encoded with Go's JSON
encoder, including inventories and dataset digests. Preserve the original
recipe with the plan. This is not the raw recipe-file digest used by the
finite-job envelope.

The materializer provides these **unqualified parameter sets**, requiring real
immutable local assets:

| Candidate | Adapter | Rank and alpha | Sequence | Microbatch and accumulation | Steps |
| --- | --- | --- | --- | --- | --- |
| Small Llama smoke model | LoRA | 8/16 | 128 | 1/1 | 2 |
| Representative 7-8B Llama | QLoRA | 16/32 | 2048 | 1/4 | 100 |

Both use learning rate 0.0002 and seed 42. The qualification guide supplies
real immutable asset revisions. Local files, datasets and explicit budgets
remain required. The tool uses no placeholder revision, fake inventory or
measured memory claim.

### Qualification still required

Compare Axolotl, TRL with PEFT and Unsloth using the same pinned base and
tokenizer, datasets, split and training settings before selecting one supported
backend. Freeze all runtime bytes and entry points, including Torch, CUDA and
kernel and GPU architecture compatibility. Record the measured RAM, VRAM and
storage privately.

The [experimental runner](TRAINING.md#experimental-llm-training-runner) adds the supervised execution
boundary, sanitized child environment, bounded progress, held-out evaluation,
fresh-process reload and complete export provenance. It still needs a
completely pinned, qualified runtime and real model and GPU evidence. The [tool
for import preparation](TRAINING.md#llm-artifact-import-preparation) validates artifact identity.
Serving compatibility and activation remain separate qualification steps. This
preparation tool still creates candidate material only. #453 remains
incomplete.

### Inspected upstream API

Configuration mapping targets Axolotl at source commit
`1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`, not a supported environment lock:

- [Local JSON and YAML loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/cli/config.py)
- [Local dataset loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/utils/data/shared.py)
- [Completion text strategy](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/prompt_strategies/completion.py)
- [Model and NF4 loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/loaders/model.py)
- [Reporting and upload configuration](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/core/builders/base.py)

## LLM artifact import preparation

`gpu-training-import` validates a local PEFT export against the original recipe
and writes a new private preparation plan. It does not copy artifacts, convert
weights, register or replace a serving model, or activate a workload.

```sh
gpu-training-import -format peft -recipe /private/recipe.json -output /private/new-import-plan.json
```

Keep the destination parent private. Publication never replaces an existing
file. The plan contains `status: "unqualified"` and `servingQualified: false`.
`peft` identifies the artifact format, not a serving runtime.

The command rejects vLLM, llama.cpp, GGUF and other target values. A PEFT reload
does not prove compatibility with any of those runtimes. Before enabling serving
import for one of them, qualify the version and a supported base and adapter
combination. Also collect evidence of supervised serving and handoff.

The runner writes provenance version 2 only after successful training,
evaluation and separate-process reload. It binds the original recipe bytes,
model and tokenizer revisions and complete inventory identities. It also binds
the runtime inventory and entrypoint, embedded worker, invocation, held-out
loss and exported files. The runner checks adapter weights, adapter configuration and
saved tokenizer files before and after reload. Added, removed or changed
participating files prevent completion. `exportBytes` now bounds their combined
size, not only adapter weights.

Supported exports contain one `adapter_model.safetensors`, an
`adapter_config.json`, a `tokenizer_config.json`, and at least one of
`tokenizer.json` or `tokenizer.model`. The optional bound files are
`special_tokens_map.json`, `added_tokens.json`, `generation_config.json`,
`config.json`, `vocab.json`, `vocab.txt`, `merges.txt` and
`chat_template.jinja`. Only these named files belong to the import plan.
Checkpoints, caches, training arguments, README files and other training outputs
are not import payloads. The command does not support sharded adapters, pickle
adapters, template directories or configuration redirects to other code or
files. The adapter must declare LoRA, causal language modeling and the precise
local base path from the recipe.

Preparation rechecks the original model, tokenizer and runtime inventories, the
full recipe digest and every exported file. It requires version 2 provenance
from the same worker implementation and source revision and rejects old
weight-only provenance. Use the same supervisor release for training and
import preparation.

Both materialization and import hash the complete model inventory, so large
models need a full read of their weights at each step. The plan is a snapshot:
consumers must revalidate before copying or loading its listed files. It is
neither a signature nor proof against a trusted owner who rewrites both data and
metadata. Private paths and inventories stay private.

This command does not import or execute a runtime. The plan excludes dataset
contents and completion credentials and does not show supervisor finalization,
GPU release, environment approval or serving fitness. #453 stays open until
those host qualification gates pass.

## LLM training qualification

#453 stays open until the real GPU evidence below passes. Repository tests,
dependency resolution, discovery and import preparation do not approve a
runtime. All training presets remain disabled. Checkpoint and resume is #454.
Training UI is #456. Keep host identities, paths, datasets, generated text and
raw evidence private.

### Environment and backend decision

The [dependency input](../training/axolotl/requirements.in) is an explicit
subset for the worker's local SFT and LoRA path, with Torch 2.14.0 and its
build tools. It omits Gradio, Trackio, s3fs, gcsfs, adlfs, ocifs, lm_eval and
openenv-core, and overrides Axolotl's python-dotenv pin. The lock requires
fsspec >=2026.6.0, Pillow >=13 and python-dotenv >=1.2.2. These are nine
deliberate deviations from Axolotl's declared requirements: eight absent
distributions and one version override.

CI checks these deviations exactly. More or changed deviations fail CI. The
worker rejects the excluded distributions. Axolotl source and its installed
dependency metadata remain unchanged. The worker uses no fork.

The CPU smoke test exercises the worker's actual setup, train, evaluate and
save sequence. It uses a tiny random model, two LoRA optimizer steps and a
separate-process reload. It runs offline after installation and requires
finite loss and logits. This verifies the subset's code path on dependency
updates, not GPU suitability.

The accompanying hash-locked requirements resolve for Linux x86_64 and CPython
3.12.14. They are a **package dependency candidate**, not a complete approved
environment. The [runtime check](#runtime-check) tests the interpreter, the
standard library, the installed versions, the native libraries and the Torch
and CUDA compatibility with the GPU. It does not record provenance or run GPU
kernels.

Source distributions need controlled builds. A source hash does not pin the
resulting wheel or its build environment. Do not install these into inference
environments.

For a dedicated test environment, install the lock before the unchanged
source, then run both checks with that environment's interpreter:

```sh
uv pip sync --python "${PYTHON:?}" --require-hashes training/axolotl/requirements.txt
uv pip install --python "$PYTHON" --no-deps --no-build-isolation \
  'axolotl @ git+https://github.com/axolotl-ai-cloud/axolotl.git@1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1'
"$PYTHON" -I -S -B training/axolotl/check_environment.py
sudo unshare --net -- timeout 120 "$PYTHON" training/axolotl/smoke.py
```

Use an absolute `PYTHON` path. Installation requires access to the pinned
artifacts. The smoke runs in a network namespace with a two-minute limit, as in
CI. Ordinary `pip check` reports the nine intentional deviations.

Do not ignore arbitrary failures or edit installed metadata: use the exact
deviation checker. The CPU reload tests adapter compatibility. The production
worker's CUDA reload and bounded generation still need real-GPU evidence.

Regenerate only as an explicit dependency update, using uv 0.12.23:

```sh
uv pip compile training/axolotl/requirements.in \
  --python-version 3.12.14 --python-platform x86_64-unknown-linux-gnu \
  --generate-hashes --no-annotate --no-header \
  --output-file training/axolotl/requirements.txt
```

Before approving installation, build or download a private wheelhouse. Record
the hashes and the exact build-tool environment. Test installation offline.
Install Axolotl noneditable with `--no-deps` from commit
`1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1` (version 0.21.0).

Preserve its real VCS `direct_url.json`. Do not falsify that metadata to
satisfy the worker. The runner needs regular `bin/python3` and
`lib/pythonX.Y/site-packages` files with complete inventories. A usual
symlinked virtualenv is not this runtime layout.

Record the approved interpreter, stdlib and external shared-library closure
separately, including hashes and provenance.

Source review on 2026-10-22 sets these constraints, not a performance
winner:

| Candidate source | Verified integration constraint | Remaining evidence |
| --- | --- | --- |
| [Axolotl 1f1eb05](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/pyproject.toml) | Python >=3.12. Transformers 5.20.1, Datasets 5.0.1, PEFT 0.21.2, TRL 1.14.1. Existing supervised worker targets this source | Complete installed closure, both recipes, resource measurements |
| [TRL 1.14.1 fd74bbc](https://github.com/huggingface/trl/blob/fd74bbc7b5f852a70d4cc94377e0a8f94392fda1/pyproject.toml) with PEFT | Python >=3.10. Datasets >=4.7.0 and Transformers >=4.56.2. Independent reference adapter still needs qualification | Equal tokenization, loss, optimizer steps, export and fresh reload |
| [Unsloth 356721b](https://github.com/unslothai/unsloth/blob/356721b31ebef3c1c0fcb8d53572c5be3d6eba89/pyproject.toml) | Its training extra caps Transformers at 5.17.0 and Datasets below 5.0.0. Cannot share this Axolotl environment | Separate resolved closure, equal recipe and measured comparison |

Run each backend in its own pinned environment, using the same values for:

- Asset revisions, dataset bytes and splits, and seed.
- LoRA targets, rank, alpha and dropout.
- BF16, sequence length, batch and accumulation.
- Optimizer, learning rate and number of optimizer steps.

Disable packing, reporting and uploads. Record the tokenization differences
explicitly. Matching row counts alone does not make the workloads equal.

Do not bypass dependency constraints to force a shared environment. The
production runner now supports only the Axolotl candidate. A TRL or
Unsloth experiment is not an accepted trainer.

#### Runtime check

Run `"$PYTHON" -I -S -B training/axolotl/check_environment.py` with the
runtime's own interpreter, as the runner does. The check refuses to run without
`-I` and `-S`, and it writes no bytecode even without `-B`. It adds the runtime
site-packages after the standard library, so no site-packages module can
replace a standard-library module inside the checker. It prints one bounded
JSON report, version 1.

The exit status is 0 for `accepted` and 1 for `rejected`. Exit status 2 has two
causes. An internal error prints the same JSON keys with the status `rejected`,
empty `checks` and an `error` text. A usage error prints argparse usage text on
standard error and no JSON. The `--target` option selects the target:

| Target | Rule |
| --- | --- |
| `cpu-smoke` (default) | Preset-only problems have the status `warn` and do not reject the CPU smoke |
| `smoke-lora` or `representative-qlora` | Each problem has the status `fail` and rejects the report |

For a preset, give `--gpu-uuid` with the trainer GPU UUID, in the same format
that the supervisor accepts. The check then sets `CUDA_VISIBLE_DEVICES` to that
GPU for its child process, as the runner does. Without it, a preset target
fails the `accelerator` check.

| Report field | Value |
| --- | --- |
| `version`, `status` | `1`, and `accepted` or `rejected` |
| `target` | The selected target, or `null` for an internal error before target selection |
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
The check scans site-packages before and after the child process. When the two
scans find different shadowing or startup-file problems, the `standard-library`
check fails. The scan does not notice changed file contents.

The runner's interpreter and the checker use `-S`, so they do not process
`.pth` files. Subprocesses that the training stack starts, such as compile
workers, can process them. Thus `startup-files` accepts only a `.pth` file that
the `RECORD` of a locked wheel lists.

The native, driver and accelerator checks run in a child process with
`-I -S -B`. The child gets the runner environment and puts the runtime
site-packages first, as the worker does. The runner drops all other variables,
such as `LD_LIBRARY_PATH`, `LD_PRELOAD` and `BNB_CUDA_VERSION`, so the child
does not get them. The check stops a child that runs longer than 300 seconds.

`uv pip sync --require-hashes` checks the integrity of the installation. The
runtime check finds version drift and wheels for another interpreter or
platform. It does not find changed or damaged files. Every target, also
`cpu-smoke`, rejects distributions that are not in the lock. Thus use
`uv pip sync`, not `uv pip install`, for the lock.

The lock installs xformers, which links `libcudart.so.12`. The lock installs no
`libcudart.so.12`, because Torch uses CUDA 13.0. The supported presets use SDPA
attention, so the check does not load xformers.

The accelerator check needs a CUDA build of Torch and a visible GPU. The driver
must support at least the CUDA major version of the Torch build.

SASS for a target runs when the target has the same major and an equal or lower
minor than the GPU. PTX runs on a GPU with an equal or higher capability. PTX
needs a driver for the Torch CUDA version or newer. An `a` target, such as
`sm_90a`, runs only on the exact capability. An `f` target, such as `sm_100f`,
runs only on the same major. The presets need native BF16, which starts at
compute capability 8.0.

When Torch has no usable GPU, the report shows `"accelerator": "cpu"`. Each
problem tells what is wrong and what to change. The report contains no dataset
content, credentials or host paths.

Before training starts, each worker phase runs the same accelerator check, so a
preset never falls back to CPU training. Run the check only in the dedicated
training runtime. Activation and inference paths do not run it.

### Immutable asset candidates

[Asset manifest](../training/assets.json) records two real source revisions:

| Preset | Model and tokenizer snapshot | Parameters |
| --- | --- | --- |
| `smoke-lora` | [TinyLlama 1.1B](https://huggingface.co/TinyLlama/TinyLlama-1.1B-intermediate-step-1431k-3T/tree/59f6f375b26bde864a6ca194a9a3044570490064) | LoRA 8/16, sequence 128, batch 1, accumulation 1, 2 steps |
| `representative-qlora` | [Llama 3.1 8B, NousResearch distribution](https://huggingface.co/NousResearch/Meta-Llama-3.1-8B/tree/1f47e50cdbe801ad8a5174156ec3a0655108fb9f) | QLoRA 16/32, sequence 2048, batch 1, accumulation 4, 100 steps |

Both presets use learning rate 0.0002 and seed 42. These weights support
training, not GGUF inference. The presets pin the source revisions. This does
not show memory fit or quality.

`assets.json` is the reference for qualification snapshots, not an input to
materialization. Presets fix training parameters. Materialization accepts an
operator-supplied full revision and hashes the local inventory. Compare both
input revisions with this table when qualifying these reference candidates.

Use the model's applicable license. Keep downloaded model and tokenizer in
separate, new directories. With an already installed download client, use its
[snapshot_download](https://huggingface.co/docs/huggingface_hub/guides/download)
API with the exact `revision` above and `local_dir`. Allow only model
`config.json`, `generation_config.json`, `model*.safetensors` and shard-index
JSON, and tokenizer `tokenizer.json`, `tokenizer.model`,
`tokenizer_config.json`, `special_tokens_map.json` and `added_tokens.json`.

Exclude pickle weights and code. Before inventorying the new download
directories, remove only the downloader's `.cache/huggingface` metadata.
Otherwise the precise asset inventory rejects them. Download and installation
remain an explicit deployment step outside activation.

### Materialize and inspect

Write a private input JSON object with exactly these fields:

| Field | Value |
| --- | --- |
| `version`, `preset` | `1`. `smoke-lora` or `representative-qlora` |
| `model`, `tokenizer` | Each has `root` (absolute local directory) and `revision` (the full snapshot SHA) |
| `runtimeRoot` | Absolute dedicated runtime directory |
| `train`, `heldOut` | Absolute, distinct local JSONL paths using the candidate text schema |
| `outputDirectory` | New, empty, private absolute directory |
| `budget` | All fields from [candidate resource estimates](TRAINING.md#resource-estimates). Explicit operator estimates, never zeros or inferred from model size |

Use absolute paths for the shell variables below. New recipe and plan files
must not already exist. Their parent directories must be private. Use different
output directories, recipes and job IDs for each training run.

```sh
gpu-training-candidate -version
gpu-training-runner -version
gpu-training-candidate -materialize "${INPUTS:?}" -output "${RECIPE:?}"
gpu-training-candidate -recipe "$RECIPE" -output "${PLAN:?}"
sha256sum "$RECIPE" "$PLAN"
```

Materialization records observed bytes and runs existing candidate validation.
It does not prove where those bytes originated. The original input and recipe
stay private. The [trainer binding](TRAINING.md#finite-training-jobs) also needs the exact unit,
executable and recipe hashes, physical GPU UUID, cgroup, distinct job and
terminal paths and a **measured** `requiredMiB`. Read-only trainer inspection
can check that binding. It cannot invent the absent measurement or approve the
environment.

### Private host inputs and evidence

Gather these before choosing a deployment lock. These commands read system
state. Redirect their output to a private directory, never a public issue or CI
log:

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
`rejected` report blocks that preset. Fix each `fail` problem and run the check
again.

Also record:

- Release and checksum, approved Python path and version and stdlib inventory.
- All installed distribution metadata and wheel and source hashes.
- External ELF loader and library identities.
- CUDA runtime, Torch build, supported GPU architecture and driver
  compatibility.
- The reviewed egress containment.
- Model and tokenizer source and inventories.
- Dataset digests and counts and recipe and plan.
- Unit bytes and loaded properties.
- Existing inference catalog and revision for handoff verification.

Do not collect raw environment dumps or lifecycle completion tokens into
reports.

After the environment and measured trainer binding are ready, run through the
supervisor, never by starting the worker or trainer unit directly. Use the
installed operator's existing state and runtime flags before each command where
required:

```sh
gpu-mode -target "${TRAINER_PROFILE:?}" -training-job "${JOB_ID:?}" training-start
gpu-mode -training-job "$JOB_ID" training-status
gpu-mode -training-job "$JOB_ID" training-reconcile
gpu-mode -training-job "$JOB_ID" training-status
```

Reconcile only after the trainer exits. Missing or invalid evidence leaves
admission pending. For interruption or uncertain launch, explicitly run
`gpu-mode -training-job "$JOB_ID" training-recover`. It stops and verifies
release, never resumes or retries training. Keep the output private for every
command.

For each backend and size, keep the following evidence privately:

| Requirement | Required observation |
| --- | --- |
| Real training | Finite forward and backward execution and exactly the requested optimizer step count |
| Quality boundary | Finite nonnegative held-out loss. Preserved split identity. No generated text in supervisor logs |
| Reload | Separate process loads the exact base, adapter and config and saved tokenizer. Finite forward output and bounded continuation |
| Capacity | Peak cgroup RAM of the trainer unit and GPU VRAM, from the sources below. Free disk throughout. Retained checkpoints plus one in-progress save, cache and export |
| Completion | Valid fenced completion accepted by reconcile. Recursive cgroup empty and GPU compute processes released |
| Failure | Controlled interruption or failure never publishes success. Recovery releases admission only after verified cleanup |
| Handoff | Existing inference profile starts through the supervisor after release and serves the expected model |
| Serving import | Explicit prepared artifact matches its manifest. Separately selected serving runtime and version loads the exact base+adapter correctly |

Sample memory usage with `nvidia-smi` throughout the job and record its sample
interval. Sampled GPU usage is a lower bound, not an exact peak.

For the RAM peak, use the sources in this order:

1. The [`memoryPeakBytes`](TRAINING.md#memory-peak) value of the job status.
   The kernel and systemd measure it for the unit cgroup. It is a lower bound.
2. Your own samples of the `memory.peak` file of the unit cgroup during the
   run.

A `memoryPeakBytes` value for every outcome needs systemd 256 or later. On
systemd 252, the `MemoryPeak` property does not exist. On systemd 255, systemd
usually does not keep the value after the unit exits. On those hosts, the
sampling in item 2 is an operator step for each qualification run. The
supervisor does not do it. See [the systemd check](TRAINING.md#memory-peak).

The [runner `peaks.json` file](TRAINING.md#resource-peaks) is only
an unverified hint, because code in the trainer unit can write it.

Capture disk usage during saves, not just after pruning. Add safety headroom to
observed values before setting capacity. Failures, unmeasured peaks, missing
evidence or unsupported combinations keep qualification incomplete.

The [tool for import preparation](TRAINING.md#llm-artifact-import-preparation) validates artifact
identity. It does not merge or convert weights, change a serving model or
activate a runtime. Keep #453 open until a reviewed deployment record covers
every real-GPU gate.

## Experimental LLM training runner

`gpu-training-runner --job-file /private/training/job.json` implements the
version 1 [finite-job adapter](TRAINING.md#finite-training-jobs). Invoke it through the supervisor's
exact trainer unit, with its bound physical GPU UUID and systemd invocation.
The runner ships without an enabled preset. #453 remains open until runtime,
backend comparison and GPU qualification pass. Checkpoint and resume is #454.

The runner accepts the existing strict candidate recipe. Its runtime entrypoint
must be Python 3.12 or newer (the pinned Axolotl minimum), with an inventoried
regular executable at `bin/python3` and packages under
`lib/pythonX.Y/site-packages`. Runtime files must belong to root or the
execution user and must not be group or other writable. Inputs still need
complete inventories, exact recipe and dataset digests, and resource preflight.
The output directory must be empty and private. Preparation never installs
dependencies.

Python runs with `-I -S -B`: no ambient Python path, user site, startup `.pth`
execution or bytecode writes. The embedded worker adds only the inventoried
runtime's site-packages. Axolotl's installed `direct_url.json` must identify
noneditable source commit `1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`.

The guard limits Axolotl entry points by owner, group, name and value. It
permits only that revision's two bundled context and expert parallel plugins
and four bundled cloud providers (CoreWeave, Nebius, Modal and Baseten). The
guard inspects this installed metadata without importing or using the
providers. Before
importing Axolotl, the guard rejects other plugins, providers and model entry
points. Bundled metadata is not permission to use parallel training or cloud
providers.

Before training starts, each phase checks Torch and CUDA against the bound GPU.
The phase needs a CUDA build of Torch, a usable driver, kernels for the GPU
architecture and native BF16. QLoRA additionally needs bitsandbytes. The configuration
value `load_in_4bit` must be a boolean. An unsupported combination stops the
phase, and training never falls back to CPU.

The runner discards the output of the worker process. To see the reason, run
the [runtime check](TRAINING.md#runtime-check).

This is a byte and inventory boundary, not a complete approved environment
lock. Python's standard library, OS and native libraries, and CUDA and driver
compatibility still need pinned and qualified provenance. The check rejects
incompatible combinations but does not pin them. A matching source metadata
file or a successful fixture is not runtime approval.

Every execution gets a new private directory beside its lifecycle envelope,
named `.llm-<invocation>`. Reusing that invocation fails. It contains the
generated configuration, embedded worker, isolated home and cache, bounded
current progress, per-phase receipts and a best-effort [resource peaks
file](#resource-peaks). The completion credential never enters the worker's
files, arguments or environment.

The runner discards child stdout and stderr because third-party messages can
contain private data. Errors report a failed phase without echoing input paths,
datasets or credentials. Keep private trainer artifacts for diagnosis.

Training uses Axolotl's pinned model and trainer setup and native Trainer APIs.
It must reach exactly the requested optimizer step, pass held-out evaluation
with a finite nonnegative loss, and save its adapter and tokenizer. A separate
Python process then loads the base and saved adapter and checks finite forward
output and a bounded deterministic continuation. No generated text enters
lifecycle evidence. Both phases stay in the trainer unit and therefore under
the same admission.

Only after both processes succeed does the runner check the bounded export,
write version 2 `supervisor-provenance.json` beside it, and atomically publish
the fenced `completed` receipt. The provenance binds source, raw recipe, model
and tokenizer revisions and inventories, runtime inventory and entrypoint,
embedded worker, invocation, exported file hashes and held-out loss. It
contains no completion token. Adapter configuration, weights and saved
tokenizer files must stay unchanged across reload.

`exportBytes` bounds their combined size. A failed phase never publishes
successful completion. Systemd failure or explicit recovery still supplies the
supervisor's failure or interruption handling. The supervisor independently
verifies cgroup and GPU release and capacity afterward.

The runner enables Hub and dataset offline modes and disables telemetry and
reporting. It omits ambient credentials. These settings are not network
isolation. Qualify the runtime and deployment restrictions before enabling a
training profile. The separate [tool for import
preparation](TRAINING.md#llm-artifact-import-preparation) validates artifact identity without
registering or activating a serving model. Serving compatibility and checkpoint
resume stay outside this runner.

See [qualification](TRAINING.md#llm-training-qualification) for dependency candidates,
immutable asset revisions, private host inputs and the real-GPU acceptance
gates.

Repository tests use an explicitly identified worker fixture for process,
environment, receipt, failure and provenance behavior. Python guard tests check
source and plugin rejection and atomic progress publication, plus the
accelerator rules with mocked Torch and CUDA values. They do not load model
weights, exercise GPU kernels or prove real training and reload compatibility.

### Resource peaks

Do not trust `peaks.json`. It is best-effort diagnostic data. Code in the
trainer unit can write or change it at any time, with or without a terminal
receipt. A failed job has no receipt by design. Use the file only as an
unverified hint. For qualification, see [the source
order](TRAINING.md#private-host-inputs-and-evidence).

After creating the private execution directory `.llm-<invocation>` beside the
lifecycle envelope, the runner writes `peaks.json` in it when the job completes
or fails. It writes no file when:

- Validation fails before the runner creates that directory.
- A signal the runner cannot handle stops it, for example an OOM kill or
  `SIGKILL`.

The runner takes one final sample per job, the cgroup peak included. A
finished run takes it before its final input checks. After the checks, the
runner publishes the provenance and then writes the receipt. A run that fails
before its final input checks takes the final sample after the failure. A run
that fails during or after those checks keeps the final sample taken before
them.

The runner writes the file once, after the outcome is final: after the
`completed` receipt or after the failure. A failed write never changes the
outcome. The terminal receipt, not the outcome in `peaks.json`, is the
authoritative outcome. The file is one JSON object of at most a few hundred
bytes. It contains no paths, dataset contents or credentials.

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `outcome` | `completed` or `failed` |
| `phase` | Last phase that started: `prepare`, `train`, `reload` or `export`. `export` is the final input check, the provenance and the receipt after reload. The progress file calls the `train` phase `training` |
| `ramPeakBytes` | Largest resident set of a single process, from `ru_maxrss` of each worker and the descendants that it reaped, across phases. It is not a sum. Use the cgroup values for the total |
| `vramPeakBytes` | Largest sample of GPU memory from the progress file of the worker |
| `storagePeakBytes` | Largest allocated size of the output directory and the execution directory |
| `cgroupMemoryPeakBytes` | `memory.peak` of the cgroup of the trainer unit, read with the final sample |
| `sampleIntervalMs` | Interval of the storage samples only |

A `null` value means the runner did not measure the value. The worker samples
GPU memory at each optimizer step, after held-out evaluation, after the save
and after reload. Each sample is the Torch allocator peak of the worker. It
excludes memory outside the allocator, such as the CUDA context, so it is a
lower bound for the trainer. Other processes on the GPU do not change it.

When a phase fails with a CUDA out-of-memory error, the worker tries to write
one last sample. Other errors make no CUDA call. A killed worker writes none.

A failed sample never fails a phase. Storage samples can miss a short peak, so
the storage value is a lower bound.

The storage sample includes retained checkpoints, a save in progress, the cache
and the export. One walk stops after 65536 entries or 10 seconds. It also stops
at an unreadable directory, at the end of a phase and at a stop signal. A walk
that stops early gives no value, because a partial total is not a usable
capacity value. If no walk finishes, `storagePeakBytes` is `null`.

The walk opens each directory from its parent directory without following
links, so a link that replaces a directory cannot redirect it. It does not
cross into another mount. A blocked filesystem call can hold one walk. At the
end of each phase, the runner waits at most 1 second for a walk in progress. It
waits at most 11 seconds for the final sample: the 10-second walk deadline plus
1 second. After that, the runner drops the walk result.

## Trainer binding inspection

`gpu-setup discover` adds a separate `trainers` array for explicit trainer
profiles in the current catalog or pending activation. Existing application
discovery does not change. An empty array means no explicit trainer bindings were
available, not that the host lacks training software.

Each result identifies the workload and unit, reports `bindingStatus`, and keeps
`qualificationStatus: "unqualified"`. A `verified` binding means the same
executable, recipe, exact unit source, loaded command and environment, and
physical GPU identity checks used before training passed. It does not approve
the Python environment, prove measured capacity, test GPU kernels or authorize
launch. Failures stay visible with generic diagnostic guidance, without
subprocess output or lifecycle credentials.

Inspection validates the request before inspecting the selected binding and
never applies it. A trainer binding must supply the complete version 4
catalog trainer profile in [finite training jobs](TRAINING.md#finite-training-jobs). This includes measured
`requiredMiB`, GPU UUID, unit, executable and recipe paths and digests, and
private lifecycle paths.
Inspection infers none of these from a service name or the job envelope.

Inspection performs file and loaded-unit observations only. It does not execute
the trainer or preparation command, start or stop services, install
dependencies, write lifecycle files, change the catalog or enable a preset. All
launch checks run again at training start. The training UI remains separate in
#456.

## Finite training jobs

Training uses a separate catalog version 4 binding. Catalog versions 1 and 2
stay supported. Version 3 remains reserved. Inference `nativeModel` bindings
cannot launch training.

The trainer adapter owns its recipe, datasets, metrics, checkpoints, evaluation
and export. All GPU work must remain inside its systemd unit's recursive
cgroup. The supervisor stores only lifecycle identity, terminal evidence and
the memory peak of the trainer unit. It does not queue jobs or resume
interrupted training automatically.

### Adapter contract, version 1

A trainer profile uses `adapter: "systemd"`, `bootPolicy: "stop-to-idle"`, a
measured nonzero `requiredMiB`, and no health or release endpoint, native model
or application launch binding. Its `trainer` object contains:

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `gpuUUID` | Full physical GPU UUID |
| `launchFile`, `launchSHA256` | Exact unit source and SHA256 |
| `executable`, `executableSHA256` | Adapter qualified by shared executable trust checks and SHA256 |
| `recipeFile`, `recipeSHA256` | Trusted local recipe and SHA256 |
| `jobFile` | Private lifecycle envelope written before launch |
| `terminalFile` | Trusted terminal evidence written by the adapter |

All paths must be distinct, absolute, clean literal paths without shell
expansion. You or root must own the adapter and its traversed ancestors, and
they must not allow world write access. The validator resolves and checks
symlinks.

The single-admin host policy trusts members of your own groups and allows group
write only for groups you belong to. Lifecycle output files cannot overlap
another trainer binding. The job directory must already exist with trusted
ownership and permissions. Keep external service management paused while a
supervisor job remains unfinished.

The accepted unit source has exactly this form, with the bound values
substituted and a final newline. The validator does not support more directives
or drop-ins:

```ini
[Unit]
Description=Supervised finite training
[Service]
Type=exec
Restart=no
KillMode=control-group
RemainAfterExit=yes
Environment=CUDA_VISIBLE_DEVICES=GPU-...
ExecStart=/trusted/trainer-adapter --job-file /private/training/job.json
```

The adapter reads the version 1 JSON envelope containing `job`, `recipeFile`
and `terminalFile`. `job` includes ID, completion token, fence, workload,
accepted catalog revision and recipe SHA256. The adapter must validate this
envelope and the recipe digest before GPU-backed preparation. Its terminal file
is one strict JSON object, at most 64 KiB (65536 bytes), with `version: 1`,
`id`, `token`, `fence`, `recipeSHA256`, `invocationId` (systemd's
`INVOCATION_ID`) and `outcome`.

Supported outcomes are `completed`, `failed`, `cancelled` and `interrupted`.
Job IDs follow the shared request ID contract: at most 8192 UTF-8 bytes and no
control characters. The terminal limit allows the full ID even when JSON
escaping expands each ID byte to six bytes, plus the remaining receipt fields.
Write evidence atomically with private permissions. Never print the token.

`completed` means every requested phase, including evaluation and export,
completed. The unit retains an exited invocation until supervisor cleanup. A
zero process exit is insufficient. A fenced systemd OOM, exit-code, signal or
core-dump failure supplies failure evidence without requiring a surviving
adapter. The supervisor still stops the unit, verifies recursive cgroup
emptiness, verifies no GPU compute processes remain, and checks capacity before
releasing admission.

Backend adapters and their environment qualification are separate integrations.
This contract does not claim checkpoint or resume support for an arbitrary
trainer. The [LLM tool for candidate
preparation](TRAINING.md#llm-training-candidate-preparation) audits local inputs and generates an
unqualified Axolotl configuration. It supplies neither this executable adapter
contract nor an enabled training profile. The separate [experimental LLM
runner](TRAINING.md#experimental-llm-training-runner) implements that execution boundary without
qualifying a runtime or enabling a profile.

Read-only [trainer inspection](TRAINING.md#trainer-binding-inspection) exposes explicit binding
validation for discovery and future setup. Follow the [LLM qualification
guide](TRAINING.md#llm-training-qualification) for candidate recipe materialization and
private host evidence. Successful inspection is not GPU qualification.

### Commands

Use the usual `gpu-mode` state and runtime flags before the command:

```sh
gpu-mode -target trainer -training-job unique-job-id training-start
gpu-mode -training-job unique-job-id training-status
gpu-mode -training-job unique-job-id training-reconcile
gpu-mode -training-job unique-job-id training-recover
```

The start command validates the catalog and output paths, then durably reserves
a unique request ID before draining opposing work. This `preparing` claim
blocks competing requester demand and work registrations but is not GPU
admission. After verified idle release, one transaction consumes the matching
claim and records job registration and single-use launch intent. The supervisor
rejects reused IDs, including IDs of interrupted preparations.

A crash during preparation is visible in status. Explicit recovery records it
as `interrupted` without stopping unrelated inference or launching the trainer.
Failed handoffs retire the claim when possible. Otherwise preparation remains
recoverable.

Routine switches, auto-idle, catalog replacement and generic recovery cannot
discard the admission. Status redacts the completion token.

Reconcile reads evidence and never starts the trainer. Before cleanup, the
supervisor durably binds a validated terminal outcome to the job. Failed
cleanup or finalization can retry without losing that outcome. Admission
remains pending until the supervisor verifies release.

Missing, stale or ambiguous evidence keeps the job pending. After a launch or
controller crash, `training-recover` explicitly stops the configured trainer
and verifies release. It records `interrupted` when no verified terminal
outcome exists in durable state. Otherwise it preserves that outcome. Recovery
never retries the launch.

If stop or release verification fails, the job stays pending. You can retry
recovery. Start a new job ID only after the prior job is terminal. A resume
integration must create a new job and fence.

#### Memory peak

Before cleanup stops the trainer unit, reconcile and recovery read the memory
peak of the unit. The job record keeps the largest value as `memoryPeakBytes`,
and status shows it. This recording is best effort: a missing peak or a failed
write never blocks the stop or the cleanup. `memoryPeakBytes` is a field of the
job record. The terminal file contract does not change.

If systemd can report the unit cgroup for the job invocation, the supervisor
keeps the larger of cgroup `memory.peak` and the `MemoryPeak` property.
Otherwise it reads only the `MemoryPeak` property. The kernel and
systemd measure the value, not the trainer. The value includes the page cache,
but only of processes in the unit cgroup. Trainer code can move processes out
of that cgroup, so the value is a lower bound and not proof against changes.

Reconcile reads the value after the trainer writes its outcome. Recovery reads
it before stopping the unit. When recovery stops a running trainer, the value
is the cgroup peak up to that time. When the unit already exited or failed,
systemd usually removed the cgroup, and the value comes from the `MemoryPeak`
property.

A value for every outcome needs systemd 256 or later. The systemd source shows
these facts for the supported versions (see [the desktop
requirements](OPERATIONS.md#local-gnome-desktop-installation)):

- systemd 252 has no `MemoryPeak` property. systemd 255 adds it.
- systemd 255 keeps the value only if something reads it before it removes the
  cgroup, so it usually gives no value after the unit exits.
- systemd 256 and later, 259 included, keep the value when they remove the
  cgroup.

Use the systemd version to decide. Run `systemctl --version`. On systemd 256 or
later, `memoryPeakBytes` has a value for every outcome. On earlier versions,
the supervisor does not sample during the run. For qualification, the
operator reads the `memory.peak` file of the unit cgroup directly during every
run, not through `systemctl`.

To confirm, use a unit that nothing queried during its run, because a query
during the run can make systemd 255 keep a value. After the unit exits, run
`systemctl --user show -p MemoryPeak -- <trainer-unit>.service`. A decimal byte
count shows that systemd kept the value. Any other output shows that it did
not: `[not set]`, `infinity`, `18446744073709551615`, an empty value or no
`MemoryPeak=` line.

Conditions that make the field absent:

- Your systemd version does not keep `MemoryPeak`.
- The unit cgroup has no memory controller.
- The unit has another invocation.
- The read or the write failed or timed out. Recording is best effort and
  leaves no log.

For qualification, follow [the source
order](TRAINING.md#private-host-inputs-and-evidence).
