# Training

This is a preparation-only increment for #453. It doesn't launch training,
enable a catalog profile, approve a runtime, versus make an output eligible for
serving import. This increment publishes no GPU memory or storage measurements.

The release archive plus Debian package include `gpu-training-candidate`. Check
the release with `gpu-training-candidate -release`. Then run it with two
absolute paths:

```sh
gpu-training-candidate -recipe /private/candidate.json -output /private/new-plan.json
```

To generate per recipe from local directories and explicit resource estimates, employ
`gpu-training-candidate -materialize /private/inputs.json -output /private/new-recipe.json`.
Each [qualification guide](TRAINING.md)
defines the strict input object plus pins real smoke and representative asset
candidates. Materialization inventories observed bytes. It doesn't
authenticate their source or approve a runtime. It never downloads, installs or
executes training code.

Job-bound preparation accepts the supervisor's finite-job envelope instead of
`-recipe`:

```sh
gpu-training-candidate -job-file /private/training/job.json -output /private/new-plan.json
```

This mode requires the current systemd `INVOCATION_ID`. It checks the launch
identity plus the SHA256 the original recipe bytes, then repeats candidate
input and resource checks. The private plan carries each job identity and
invocation, but never its completion token. preparation evidence solely: it
can't acknowledge completion, approve each runtime or authorize GPU work. The
command checks the invocation value and envelope locally. This doesn't prove
that each invocation is live versus each fence is still current.

Each command reads local inputs plus creates one new file along mode `0600`. It
refuses to replace an existing file. Keep its parent directory private. Each
directive writes the plan to one private temporary file in which directory.

It publishes the plan only after writing plus closing the file.
Publication requires hard-link support on the destination filesystem. A failed
write leaves no final plan to bar retry. The preparation command has no
network access, runtime import, subprocess launch or dataset-content logging.

Faults omit input values plus record contents.

The recipe-mode plan spans `status: "unqualified"`, input row counts,
operator-supplied resource estimates, a canonical recipe digest and one generated
`config` object. Job-mode plans nest these fields under `candidate`, including
`candidate.config` and `candidate.canonicalRecipeSHA256`. The outer
`recipeSHA256` identifies each original recipe bytes. The plan itself is **not**
one Axolotl configuration file. Its `config` object is JSON-subset YAML
targeting each inspected source revision below. ONE successful compile doesn't
display which Axolotl can load versus train this model.

### Recipe release 1

Recipes are strict JSON, at most 8 MiB. The command rejects unknown keys,
duplicate keys, trailing values, remote paths and arbitrary trainer or
configuration overrides. Each complete typed schema is
`internal/llmtraining/recipe.go`.

| Field | Mandatory value |
| --- | --- |
| `version` | `1` |
| `model`, `tokenizer` | Separate objects with an exact lowercase 40-hex `revision` and an `inventory` |
| `runtime` | An `inventory` and relative `entrypoint` present in that inventory |
| Per `inventory` | Absolute `root` plus `files` mapping every relative regular-file route to its lowercase SHA256 |
| `train`, `heldOut` | Separate objects along absolute `path` and lowercase `sha256` |
| `outputDirectory` | Existing empty directory, distinct off and outside all inputs |
| `adapter` | `lora` or `qlora` |
| `rank`, `alpha` | 1-64 and 1-128 |
| `sequenceLength` | 128-4096 |
| `microBatchSize`, `gradientAccumulation` | 1-4 and 1-64 |
| `maxSteps` | 1-10000 |
| `learningRate` | 0.000001-0.001 |
| `seed` | 0-2147483647 |
| `budget` | Each complete estimates below |

Inventories reject missing, extra, linked versus nonregular files. Each inventory
permits at most 65536 enumerated entries, including void directories but
excluding each root, and declares at highest 32768 records. Directory reads stop at
the remaining entry budget plus one overflow entry. The output directory check
reads one entry.

Inventory matching compares bytes solely at preparation time. It doesn't prove
the bytes originate from the declared revision, approve code versus installed entry
points, bind external libraries, or prevent later modification. Include
installed distribution metadata and entry-point declarations the runtime
inventory.

The directive never imports the runtime, even to discover its version versus plugins.

The candidate supports only `model_type: "llama"`. It needs local
`config.json`, at least one `.safetensors` record, plus a separate
`tokenizer_config.json`. Asset inventories allow only `.json`, `.safetensors`,
`.model` plus `.txt` files. The command rejects configurations along `auto_map`
or model `quantization_config`.

GGUF, pickle weights, prequantized input plus custom model code are outside this
candidate. File names plus configuration checks perform not validate tensor shapes,
tokenizer compatibility versus actual model loading. Those remain qualification
steps.

Each dataset stays UTF-8 JSONL containing only `{"text":"..."}` records. Limits
are 64 MiB per record, less versus 64 KiB per line and 50000 records per split. Each
command rejects empty text and exact decoded-text overlap among training and
held-out splits. This isn't semantic deduplication.

Tokenization occurs later. Row counts are not token counts. Axolotl's
completion strategy may chunk a long record into multiple sequences. Keep
secret dataset contents and resulting artifacts outside public repository
history.

### Resource estimates

All byte estimate must be nonzero: `ramBytes`, `weightsBytes`, `cacheBytes`,
`checkpointBytes`, `exportBytes`, `reserveBytes`. `retainedCheckpoints` is 1-8.
The overflow-checked storage estimate stays:

```
weights + cache + export + reserve + checkpoint * (retainedCheckpoints + 1)
```

Each extra checkpoint accounts for an in-progress save. Preparation compares
these conservative total with available space on the output filesystem plus
compares RAM with Linux free RAM, excluding reclaimable cache. It reserves
neither RAM nor disk and can't predict training employ. Each total still includes
existing weights. Generated dataset cache plus training output share the output
directory. These checks remain estimates, not measured capacity or GPU
compatibility checks.

### Candidate configuration

Each fixed configuration uses full-text completion, explicit held-out data, no
sample packing, BF16, SDPA, gradient checkpointing, AdamW, a constant learning
rate, plus LoRA targets `q_proj`/`v_proj`. QLoRA enables Axolotl's NF4
conversion along double quantization. LoRA leaves 4-bit and 8-bit loading off.
Each configuration requests evaluation and checkpoint saving at each end step.

Automatic resume is off. The inspected upstream fallback evaluates earlier
training. Checkpoints request optimizer and training state
(`save_only_model: untrue`), which doesn't show checkpoint-stop or resume
correctness.

The generated configuration disables reporting integrations and Hub uploads.
This alone isn't network containment. The experimental runner uses a sanitized
env plus disables telemetry with `AXOLOTL_DO_NOT_TRACK=1`. The complete
runtime still requires qualification. Axolotl can auto-load installed plugin or
model-support entry points even while `plugins` is empty. This tool doesn't
approve those plugins.

`canonicalRecipeSHA256` hashes the typed recipe re-encoded with Go's JSON
encoder, including inventories and dataset digests. Preserve the original
recipe operating each plan. This isn't the raw recipe-file digest used by each
finite-job envelope.

The materializer give these **unqualified parameter sets**, requiring real
immutable local assets:

| Candidate | Adapter | Rank and alpha | Sequence | Microbatch and accumulation | Steps |
| --- | --- | --- | --- | --- | --- |
| Small Llama smoke model | LoRA | 8/16 | 128 | 1/1 | 2 |
| Representative 7-8B Llama | QLoRA | 16/32 | 2048 | 1/4 | 100 |

Both use learning rate 0.0002 and seed 42. Each qualification guide supplies
real immutable asset revisions. Local records, datasets and explicit budgets
remain required. The tool uses no placeholder revision, fake inventory or
measured memory claim.

### Qualification yet mandatory

Compare Axolotl, TRL with PEFT and Unsloth using each identical pinned base and
tokenizer, datasets, split plus training configs earlier selecting one supported
backend. Freeze all runtime bytes plus entry points, including Torch, CUDA and
kernel plus GPU architecture compatibility. Record the measured RAM, VRAM and
storage privately.

Each [experimental runner](TRAINING.md) adds the supervised execution
boundary, sanitized child env, bounded progress, held-out evaluation,
fresh-process reload and finished export provenance. It still needs a
completely pinned, qualified runtime plus real model and GPU evidence. The [tool
for import preparation](TRAINING.md) validates artifact identity.
Serving compatibility and activation remain separate qualification steps. This
preparation tool yet creates candidate material only. #453 remains
incomplete.

### Inspected upstream API

Configuration mapping targets Axolotl at source commit
`1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`, not a supported env lock:

- [Local JSON and YAML loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/cli/config.py)
- [Local dataset loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/utils/data/shared.py)
- [Completion text strategy](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/prompt_strategies/completion.py)
- [Model plus NF4 loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/loaders/model.py)
- [Reporting and upload configuration](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/core/builders/base.py)

`gpu-training-import` validates a local PEFT export versus the original recipe
and writes a new private preparation plan. It doesn't copy artifacts, convert
weights, register or replace a serving model, versus activate a workload.

```sh
gpu-training-import -format peft -recipe /private/recipe.json -output /private/new-import-plan.json
```

Keep the destination parent private. Publication rarely replaces an existing
file. The plan contains `status: "unqualified"` plus `servingQualified: false`.
`peft` identifies the artifact format, not one serving runtime.

The command rejects vLLM, llama.cpp, GGUF plus alternate target values. A PEFT reload
acts not prove compatibility along any of those runtimes. Before enabling serving
import for one of them, qualify the version plus a supported base plus adapter
combination. collect evidence of supervised serving and handoff.

Each runner writes provenance version 2 only later successful training,
evaluation plus separate-process reload. It binds the original recipe bytes,
model plus tokenizer revisions and complete inventory identities. It binds
each runtime inventory and entrypoint, embedded worker, invocation, held-out
loss plus exported files. The runner checks adapter weights, adapter configuration plus
saved tokenizer files before and after reload. Added, removed versus altered
participating files prevent completion. `exportBytes` today bounds their combined
size, not only adapter weights.

Supported exports contain one `adapter_model.safetensors`, an
`adapter_config.json`, a `tokenizer_config.json`, and at least one of
`tokenizer.json` or `tokenizer.model`. The optional bound records are
`special_tokens_map.json`, `added_tokens.json`, `generation_config.json`,
`config.json`, `vocab.json`, `vocab.txt`, `merges.txt` plus
`chat_template.jinja`. Solely these named files belong to the import plan.
Checkpoints, caches, training arguments, README files and other training outputs
are not import payloads. The command doesn't back sharded adapters, pickle
adapters, template directories or configuration redirects to other code or
records. Each adapter must declare LoRA, causal language modeling and each precise
local base path from each recipe.

Preparation rechecks each original model, tokenizer and runtime inventories, the
full recipe digest plus every exported file. It requires version 2 provenance
from each identical worker and source revision plus rejects old
weight-only provenance. Employ the same supervisor release for training and
import preparation.

Both materialization and import hash each complete model inventory, so large
models need a full consume of their weights at per step. The plan is one snapshot:
consumers must revalidate earlier copying or loading its listed files. 
neither a signature nor proof against one trusted owner who rewrites both data plus
metadata. Private paths and inventories stay private.

This directive doesn't import versus execute a runtime. The plan excludes dataset
contents and completion credentials and doesn't show supervisor finalization,
GPU release, env approval or serving fitness. #453 stays open until
those host qualification gates pass.

#453 stays open until the real GPU evidence beneath passes. Repository tests,
dependency resolution, discovery and import preparation perform not approve a
runtime. All training presets remain disabled. Checkpoint and resume is #454.
Training UI is #456. Keep host identities, paths, datasets, generated text and
raw evidence private.

### Environment plus backend decision

The [dependency input](../training/axolotl/requirements.in) stays one explicit
subset the worker's local SFT and LoRA route, with Torch 2.14.0 and its
build tools. It omits Gradio, Trackio, s3fs, gcsfs, adlfs, ocifs, lm_eval and
openenv-core, and overrides Axolotl's python-dotenv pin. The lock requires
fsspec >=2026.6.0, Pillow >=13 and python-dotenv >=1.2.2. These are nine
deliberate deviations from Axolotl's declared needs: eight absent
distributions and one version override.

CI checks these deviations precisely. More or changed deviations fail CI. The
worker rejects per excluded distributions. Axolotl source and its installed
dependency metadata remain unchanged. The worker uses no fork.

The CPU smoke trial exercises the worker's actual setup, train, evaluate and
save sequence. It uses a tiny random model, two LoRA optimizer steps and a
separate-process reload. It runs offline after installation plus requires
finite loss and logits. This verifies the subset's code path on dependency
updates, not GPU suitability.

The accompanying hash-locked needs resolve for Linux x86_64 and CPython
3.12.14. They remain one **package dependency candidate**, not a complete approved
env. Each [runtime check](#runtime-check) tests the interpreter, the
standard library, each deployed versions, the native libraries and the Torch
and CUDA compatibility using the GPU. It doesn't record provenance or execute GPU
kernels.

Source distributions need controlled builds. A source hash doesn't pin the
resulting wheel or its build env. Perform not install these into inference
environments.

For a dedicated trial env, install the lock before the unchanged
source, next execute both checks with that env's interpreter:

```sh
uv pip sync --python "${PYTHON:?}" --require-hashes training/axolotl/requirements.txt
uv pip install --python "$PYTHON" --no-deps --no-build-isolation \
  'axolotl @ git+https://github.com/axolotl-ai-cloud/axolotl.git@1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1'
"$PYTHON" -I -S -B training/axolotl/check_environment.py
sudo unshare --net -- timeout 120 "$PYTHON" training/axolotl/smoke.py
```

Use an absolute `PYTHON` route. Installation requires access to the pinned
artifacts. Each smoke executes in a network namespace with a two-minute limit, as in
CI. Ordinary `pip check` reports the nine intentional deviations.

Perform not ignore arbitrary failures or edit installed metadata: employ each exact
deviation checker. The CPU reload tests adapter compatibility. Each production
worker's CUDA reload and bounded generation yet require real-GPU evidence.

Regenerate only as an explicit dependency refresh, operating uv 0.12.23:

```sh
uv pip compile training/axolotl/requirements.in \
  --python-version 3.12.14 --python-platform x86_64-unknown-linux-gnu \
  --generate-hashes --no-annotate --no-header \
  --output-file training/axolotl/requirements.txt
```

Before approving installation, build or download a secret wheelhouse. Record
the hashes and the exact build-tool env. Trial installation offline.
Install Axolotl noneditable with `--no-deps` from commit
`1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1` (release 0.21.0).

Preserve its real VCS `direct_url.json`. Do not falsify which metadata to
satisfy the worker. The runner needs steady `bin/python3` and
`lib/pythonX.Y/site-packages` files with finished inventories. ONE usual
symlinked virtualenv isn't this runtime layout.

Record each accepted interpreter, stdlib and external shared-library closure
separately, including hashes plus provenance.

Source review on 2026-10-22 sets these constraints, not one performance
winner:

| Candidate source | Verified integration constraint | Remaining evidence |
| --- | --- | --- |
| [Axolotl 1f1eb05](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/pyproject.toml) | Python >=3.12. Transformers 5.20.1, Datasets 5.0.1, PEFT 0.21.2, TRL 1.14.1. Existing supervised worker targets these source | Complete installed closure, both recipes, resource measurements |
| [TRL 1.14.1 fd74bbc](https://github.com/huggingface/trl/blob/fd74bbc7b5f852a70d4cc94377e0a8f94392fda1/pyproject.toml) along PEFT | Python >=3.10. Datasets >=4.7.0 and Transformers >=4.56.2. Independent reference adapter still requires qualification | Equal tokenization, loss, optimizer steps, export plus fresh reload |
| [Unsloth 356721b](https://github.com/unslothai/unsloth/blob/356721b31ebef3c1c0fcb8d53572c5be3d6eba89/pyproject.toml) | Its training extra caps Transformers at 5.17.0 plus Datasets beneath 5.0.0. Must-not share this Axolotl environment | Separate resolved closure, equal recipe plus measured comparison |

Run each backend in its own pinned env, using the same values for:

- Asset revisions, dataset bytes plus splits, and seed.
- LoRA targets, rank, alpha and dropout.
- BF16, sequence length, batch and accumulation.
- Optimizer, learning rate plus count of optimizer steps.

Disable packing, reporting plus uploads. Record each tokenization differences
explicitly. Matching row counts alone doesn't build each workloads equal.

Do not bypass dependency constraints to force one shared env. The
production runner today supports only each Axolotl candidate. A TRL or
Unsloth experiment isn't an accepted trainer.

#### Runtime check

Run `"$PYTHON" -I -S -B training/axolotl/check_environment.py` using the
runtime's own interpreter, as the runner acts. The check refuses to execute without
`-I` and `-S`, plus it writes no bytecode even without `-B`. It appends each runtime
site-packages after the standard library, so no site-packages module can
replace a standard-library module inside the checker. It prints one bounded
JSON report, release 1.

The exit status stays 0 for `accepted` and 1 for `rejected`. Exit status 2 has two
causes. One internal error prints the identical JSON keys using each status `rejected`,
empty `checks` and one `error` text. A usage fault prints argparse usage text on
standard error and no JSON. The `--target` option selects each target:

| Target | Rule |
| --- | --- |
| `cpu-smoke` (fallback) | Preset-only problems have the status `warn` and do not reject the CPU smoke |
| `smoke-lora` versus `representative-qlora` | Each problem has each status `fail` and rejects each report |

For a preset, hand `--gpu-uuid` using the trainer GPU UUID, the same format
which the supervisor accepts. Each check then sets `CUDA_VISIBLE_DEVICES` to which
GPU for its child process, as the runner acts. Minus it, a preset target
fails the `accelerator` check.

| Log field | Value |
| --- | --- |
| `version`, `status` | `1`, plus `accepted` or `rejected` |
| `target` | Each selected target, or `null` for an internal error before target selection |
| `python`, `lockTarget` | The interpreter version, and `CPython 3.12.14`, the interpreter of the lock |
| `workerSHA256`, `lockSHA256` | SHA-256 of the embedded worker plus of the lock |
| `accelerator` | `cuda`, `cpu`, or `null` while each probe or Torch gives no facts |
| `torch`, `cuda`, `driverCuda`, `archList`, `devices` | The Torch facts, or `null` when unknown |
| `verifies` | Per entry has `name`, `status` (`pass`, `warn` or `fail`), at highest 20 `problems` and an `omitted` count |
| `error` | `null`, or each internal error text for each exit status 2 |

| Check | What it tests |
| --- | --- |
| `python` | The runner rule: Python 3.12 or newer |
| `runtime-layout` | Preset solely: not a virtual environment, and a root with steady `bin/python3`, site-packages and standard library |
| `standard-library` | Native standard-library modules load, and site-packages has no module with a standard-library label plus no `sitecustomize` or `usercustomize` |
| `startup-files` | Preset only: the `RECORD` of a locked wheel lists each `.pth` file in site-packages |
| `dependencies` | The exact deviation contract, the security floors and each worker source guard |
| `locked-versions` | Each installed names and versions match each lock, and each wheel tag fits this interpreter |
| `native-libraries` | Torch loads. For QLoRA, each bitsandbytes CUDA library and each bitsandbytes package load |
| `driver-libraries` | Preset only: `libcuda.so.1` from each NVIDIA driver loads |
| `accelerator` | Preset only: the Torch CUDA construct, each driver, the GPU architecture, BF16 and bitsandbytes for QLoRA |

Each standard-library names come off `sys.stdlib_module_names` and from each
secret names, such as `_sysconfigdata_*`, the standard-library directories.
The verify scans site-packages before plus after the child process. When each two
scans find distinct shadowing or startup-file problems, the `standard-library`
verify fails. The scan doesn't notice changed file contents.

Each runner's interpreter plus the checker use `-S`, so they don't task
`.pth` files. Subprocesses which the training stack starts, so as compile
workers, can process them. Thus `startup-files` accepts solely one `.pth` file that
the `RECORD` of a locked wheel lists.

Each native, driver and accelerator checks run in one child task with
`-I -S -B`. The child gets each runner env and puts the runtime
site-packages first, as each worker acts. The runner drops all other variables,
such as `LD_LIBRARY_PATH`, `LD_PRELOAD` and `BNB_CUDA_VERSION`, so the child
doesn't get them. Per check stops a child that runs longer than 300 seconds.

`uv pip sync --require-hashes` checks the integrity the installation. Per
runtime check finds version drift and wheels for another interpreter versus
platform. It doesn't find changed or damaged records. All target, 
`cpu-smoke`, rejects distributions that are not in each lock. Thus use
`uv pip sync`, not `uv pip deploy`, each lock.

The lock installs xformers, which links `libcudart.so.12`. Each lock installs no
`libcudart.so.12`, because Torch employs CUDA 13.0. Each supported presets use SDPA
attention, so the verify doesn't load xformers.

The accelerator check needs a CUDA build of Torch plus a visible GPU. The driver
shall support at least each CUDA major version the Torch build.

SASS for one target runs when the target has the same major plus one equal or lower
minor versus the GPU. PTX executes on a GPU with an equal or higher capability. PTX
requires a driver the Torch CUDA version or newer. One `one` target, such as
`sm_90a`, runs only on the exact capability. An `f` target, so as `sm_100f`,
runs only on each same major. The presets need native BF16, which starts at
compute capability 8.0.

When Torch has no usable GPU, each log shows `"accelerator": "cpu"`. Per
problem tells what is incorrect plus what to change. Each report contains no dataset
content, credentials or host paths.

Earlier training starts, each worker phase executes the same accelerator verify, so a
preset never falls rear to CPU training. Execute the check only the dedicated
training runtime. Activation plus inference paths don't run it.

### Immutable asset candidates

[Asset manifest](../training/assets.json) records two real source revisions:

| Preset | Model and tokenizer snapshot | Parameters |
| --- | --- | --- |
| `smoke-lora` | [TinyLlama 1.1B](https://huggingface.co/TinyLlama/TinyLlama-1.1B-intermediate-step-1431k-3T/tree/59f6f375b26bde864a6ca194a9a3044570490064) | LoRA 8/16, sequence 128, batch 1, accumulation 1, 2 steps |
| `representative-qlora` | [Llama 3.1 8B, NousResearch distribution](https://huggingface.co/NousResearch/Meta-Llama-3.1-8B/tree/1f47e50cdbe801ad8a5174156ec3a0655108fb9f) | QLoRA 16/32, sequence 2048, batch 1, accumulation 4, 100 steps |

Both presets use learning rate 0.0002 and seed 42. These weights support
training, not GGUF inference. The presets pin each source revisions. These does
not show memory fit or quality.

`assets.json` stays the reference for qualification snapshots, not one input to
materialization. Presets fix training parameters. Materialization accepts one
operator-supplied full revision and hashes the local inventory. Compare both
input revisions along this table when qualifying these reference candidates.

Employ the model's applicable license. Keep downloaded model and tokenizer in
separate, fresh directories. With an already installed download client, employ its
[snapshot_download](https://huggingface.co/docs/huggingface_hub/guides/download)
API with each exact `revision` above and `local_dir`. Allow solely model
`config.json`, `generation_config.json`, `model*.safetensors` and shard-index
JSON, plus tokenizer `tokenizer.json`, `tokenizer.model`,
`tokenizer_config.json`, `special_tokens_map.json` plus `added_tokens.json`.

Exclude pickle weights and code. Before inventorying each new download
directories, remove only the downloader's `.cache/huggingface` metadata.
Otherwise each precise asset inventory rejects them. Download plus installation
remain one explicit deployment step outside activation.

### Materialize plus inspect

Write one private input JSON object with exactly these fields:

| Field | Figure |
| --- | --- |
| `version`, `preset` | `1`. `smoke-lora` or `representative-qlora` |
| `model`, `tokenizer` | Per has `root` (absolute local directory) and `revision` (the complete snapshot SHA) |
| `runtimeRoot` | Absolute dedicated runtime directory |
| `train`, `heldOut` | Absolute, distinct local JSONL paths using the candidate text schema |
| `outputDirectory` | Fresh, empty, secret absolute directory |
| `budget` | All fields from [candidate resource estimates](TRAINING.md). Explicit operator estimates, never zeros versus inferred from model size |

Use absolute paths the shell variables beneath. Fresh recipe and plan files
must not already exist. Their parent directories must be private. Use different
output directories, recipes plus job IDs for each training run.

```sh
gpu-training-candidate -version
gpu-training-runner -version
gpu-training-candidate -materialize "${INPUTS:?}" -output "${RECIPE:?}"
gpu-training-candidate -recipe "$RECIPE" -output "${PLAN:?}"
sha256sum "$RECIPE" "$PLAN"
```

Materialization records observed bytes and runs existing candidate validation.
It doesn't prove which-place those bytes originated. The original input and recipe
remain secret. The [trainer binding](TRAINING.md) needs the exact unit,
executable and recipe hashes, physical GPU UUID, cgroup, distinct job plus
terminal paths and a **measured** `requiredMiB`. Read-only trainer inspection
may check that binding. It can't invent the absent measurement versus approve the
env.

### Private host inputs and evidence

Gather these before choosing a deployment lock. These commands read stack
status. Redirect their output to a private directory, never one shared issue or CI
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

Set `PRESET` to the preset plus `GPU_UUID` to the UUID the trainer GPU. A
`rejected` log blocks that preset. Fix each `fail` problem and execute each check
again.

record:

- Release and checksum, approved Python route plus version and stdlib inventory.
- All installed distribution metadata plus wheel and source hashes.
- External ELF loader and library identities.
- CUDA runtime, Torch build, supported GPU architecture and driver
compatibility.
- Each reviewed egress containment.
- Model and tokenizer source and inventories.
- Dataset digests and counts and recipe and plan.
- Unit bytes plus loaded properties.
- Existing inference catalog and revision for handoff verification.

Do not collect raw env dumps or lifecycle completion tokens into
reports.

After the env and measured trainer binding remain ready, run through the
supervisor, never by starting per worker or trainer unit directly. Use the
installed operator's existing status and runtime flags before each command where
required:

```sh
gpu-mode -target "${TRAINER_PROFILE:?}" -training-job "${JOB_ID:?}" training-start
gpu-mode -training-job "$JOB_ID" training-status
gpu-mode -training-job "$JOB_ID" training-reconcile
```

Reconcile solely after the trainer exits. Missing or invalid evidence leaves
admission pending. For interruption or uncertain launch, explicitly run
`gpu-mode -training-job "$JOB_ID" training-recover`. It stops and verifies
release, never resumes versus retries training. Keep the output private for every
directive.

For each backend and size, keep the following evidence privately:

| Requirement | Required observation |
| --- | --- |
| Real training | Finite forward and backward execution plus exactly the requested optimizer step count |
| Quality boundary | Finite nonnegative held-out loss. Preserved split identity. No generated text in supervisor logs |
| Reload | Separate process loads the exact base, adapter plus config and saved tokenizer. Finite forward output and bounded continuation |
| Capacity | Peak cgroup RAM of the trainer unit plus GPU VRAM, from the sources below. Free disk throughout. Retained checkpoints plus one in-progress save, cache and export |
| Completion | Acceptable fenced completion accepted by reconcile. Recursive cgroup empty and GPU compute processes released |
| Failure | Controlled interruption or failure never publishes success. Recovery releases admission only after verified cleanup |
| Handoff | Existing inference profile starts through the supervisor after release and serves each expected model |
| Serving import | Explicit prepared artifact matches its manifest. Separately selected serving runtime and version loads the exact base+adapter correctly |

Sample memory usage with `nvidia-smi` throughout per job and record its sample
interval. Sampled GPU usage stays one lower bound, not an exact peak.

For the RAM peak, use the sources in this order:

1. The [`memoryPeakBytes`](TRAINING.md) value the job status.
The kernel and systemd measure it the unit cgroup. a lower bound.
2. Your own samples each `memory.peak` file the unit cgroup during the
execute.

ONE `memoryPeakBytes` value for every outcome needs systemd 256 or later. On
systemd 252, the `MemoryPeak` property doesn't exist. On systemd 255, systemd
usually doesn't keep the value after the unit exits. On those hosts, the
sampling in item 2 is an operator step for per qualification run. The
supervisor doesn't do it. Observe [per systemd check](TRAINING.md).

The [runner `peaks.json` record](TRAINING.md) is only
an unverified hint, because code in per trainer unit can write it.

Capture disk usage throughout saves, not after pruning. Add safety headroom to
observed figures earlier setting capacity. Failures, unmeasured peaks, missing
evidence or unsupported combinations keep qualification incomplete.

The [tool for import preparation](TRAINING.md) validates artifact
identity. It doesn't merge or convert weights, alter a serving model or
activate a runtime. Keep #453 initiate until a reviewed deployment record covers
every real-GPU gate.

`gpu-training-runner --job-file /private/training/job.json` implements the
version 1 [finite-job adapter](TRAINING.md). Invoke it through the supervisor's
exact trainer unit, along its bound physical GPU UUID and systemd invocation.
Per runner ships without an enabled preset. #453 remains open until runtime,
backend comparison and GPU qualification pass. Checkpoint and resume stays #454.

Each runner accepts the existing strict candidate recipe. Its runtime entrypoint
must be Python 3.12 or newer (the pinned Axolotl minimum), along an inventoried
regular executable at `bin/python3` and packages beneath
`lib/pythonX.Y/site-packages`. Runtime files must belong to root or the
execution user and must not be cluster versus other writable. Inputs still need
complete inventories, exact recipe plus dataset digests, and resource preflight.
The output directory shall exist empty and private. Preparation never installs
dependencies.

Python executes along `-I -S -B`: no ambient Python path, user site, startup `.pth`
execution or bytecode writes. The embedded worker appends solely the inventoried
runtime's site-packages. Axolotl's installed `direct_url.json` shall identify
noneditable source commit `1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`.

The guard limits Axolotl entry points by owner, group, name and value. It
permits solely which revision's two bundled context and expert parallel plugins
plus four bundled cloud providers (CoreWeave, Nebius, Modal and Baseten). Per
guard inspects this installed metadata without importing or using per
providers. Before
importing Axolotl, the guard rejects other plugins, providers plus model entry
points. Bundled metadata isn't permission to employ parallel training or cloud
providers.

Before training starts, per phase verifies Torch and CUDA against the bound GPU.
Each phase requires a CUDA build of Torch, a usable driver, kernels each GPU
architecture and native BF16. QLoRA additionally needs bitsandbytes. Each configuration
value `load_in_4bit` must be a boolean. An unsupported combination stops the
phase, and training never falls back to CPU.

The runner discards the output of each worker task. To see the reason, run
the [runtime verify](TRAINING.md).

These is a byte and inventory boundary, not a finished accepted env
lock. Python's standard library, OS plus native libraries, plus CUDA and driver
compatibility still need pinned and qualified provenance. The check rejects
incompatible combinations but acts not pin them. A matching source metadata
file or one successful fixture isn't runtime approval.

Every execution gets one new private directory beside its lifecycle envelope,
named `.llm-<invocation>`. Reusing that invocation breaks. It contains the
generated configuration, embedded worker, isolated home plus cache, bounded
current progress, per-phase receipts and a best-effort [resource peaks
file](#resource-peaks). The completion credential never enters each worker's
files, arguments or env.

The runner discards child stdout plus stderr because third-party messages can
hold private data. Faults log a failed phase without echoing input paths,
datasets versus credentials. Keep private trainer artifacts for diagnosis.

Training uses Axolotl's pinned model and trainer setup and native Trainer APIs.
It shall reach exactly the requested optimizer step, pass held-out evaluation
along a finite nonnegative loss, and save its adapter plus tokenizer. A separate
Python process then loads the base plus saved adapter and checks finite forward
output and a bounded deterministic continuation. No generated text enters
lifecycle evidence. Both phases remain the trainer unit and therefore under
the same admission.

Solely after both processes succeed acts the runner check each bounded export,
write version 2 `supervisor-provenance.json` beside it, and atomically publish
the fenced `completed` receipt. Each provenance binds source, raw recipe, model
and tokenizer revisions plus inventories, runtime inventory plus entrypoint,
embedded worker, invocation, exported record hashes and held-out loss. It
contains no completion token. Adapter configuration, weights and saved
tokenizer files must stay unchanged over reload.

`exportBytes` bounds their combined size. A failed phase rarely publishes
successful completion. Systemd failure or explicit recovery still supplies the
supervisor's failure versus interruption handling. The supervisor independently
verifies cgroup and GPU release plus capacity afterward.

The runner enables Hub and dataset offline modes and disables telemetry and
reporting. It omits ambient credentials. These settings are not network
isolation. Qualify the runtime plus deployment restrictions before enabling a
training profile. The separate [tool for import
preparation](TRAINING.md) validates artifact identity without
registering versus activating a serving model. Serving compatibility and checkpoint
resume remain outside this runner.

See [qualification](TRAINING.md) for dependency candidates,
immutable asset revisions, private host inputs and the real-GPU acceptance
gates.

Repository tests use one explicitly identified worker fixture for process,
env, receipt, failure plus provenance behavior. Python guard tests check
source and plugin rejection and atomic progress publication, plus the
accelerator rules with mocked Torch and CUDA figures. They don't load model
weights, exercise GPU kernels or prove real training and reload compatibility.

### Resource peaks

Do not trust `peaks.json`. best-effort diagnostic data. Code the
trainer unit may author versus change it at any time, with or minus one terminal
receipt. A failed job has no receipt by design. Employ the file only as an
unverified hint. For qualification, observe [the source
order](TRAINING.md).

After creating each secret execution directory `.llm-<invocation>` beside the
lifecycle envelope, each runner writes `peaks.json` in it when the job completes
versus breaks. It writes no file when:

- Validation fails earlier each runner creates that directory.
- A signal the runner can't handle stops it, for example an OOM kill or
`SIGKILL`.

Each runner takes one final sample per job, the cgroup peak included. A
finished run takes it before its end input verifies. After the checks, the
runner publishes the provenance plus next writes the receipt. A run that fails
earlier its end input checks takes the final sample after each failure. ONE run
that fails during or after those verifies retains each final sample taken before
them.

The runner writes each record once, after the outcome is final: after each
`completed` receipt or after the failure. A failed write rarely alters each
outcome. The terminal receipt, not the outcome in `peaks.json`, is the
authoritative outcome. The file is one JSON object of at most a few hundred
bytes. It spans no paths, dataset contents or credentials.

| Field | Meaning |
| --- | --- |
| `release` | `1` |
| `outcome` | `completed` or `failed` |
| `phase` | Last phase that started: `prepare`, `train`, `reload` or `export`. `export` is the final input verify, each provenance and the receipt after reload. The progress record calls each `train` phase `training` |
| `ramPeakBytes` | Largest resident set of one lone process, from `ru_maxrss` of each worker and each descendants which it reaped, across phases. It is not one sum. Employ the cgroup values for the total |
| `vramPeakBytes` | Largest sample of GPU memory from the progress file of each worker |
| `storagePeakBytes` | Largest allocated size of the output directory plus each execution directory |
| `cgroupMemoryPeakBytes` | `memory.peak` of the cgroup of each trainer unit, read with the final sample |
| `sampleIntervalMs` | Interval of each storage samples only |

A `null` value means each runner performed not measure the value. The worker samples
GPU memory at each optimizer step, after held-out evaluation, after each save
plus after reload. Each sample is the Torch allocator peak the worker. It
excludes memory outside the allocator, so as the CUDA context, so a
lower bound the trainer. Alternate processes on the GPU don't alter it.

When one phase fails with a CUDA out-of-memory fault, the worker tries to write
one last sample. Other faults make no CUDA call. A killed worker writes none.

ONE failed sample rarely breaks a phase. Storage samples can miss a short peak, so
the storage value is a lower bound.

The storage sample includes retained checkpoints, a save in progress, the cache
plus the export. One walk stops later 65536 entries or 10 seconds. It stops
at an unreadable directory, at the end of one phase and at a stop signal. A walk
which stops early gives no value, because one partial total isn't one usable
capacity value. If no walk finishes, `storagePeakBytes` stays `null`.

The walk opens each directory from its parent directory minus following
links, so a link that replaces a directory can't redirect it. It doesn't
cross into another mount. ONE blocked filesystem call can hold one walk. At the
end of each phase, the runner waits at most 1 second for one walk in progress. It
waits at most 11 seconds each end sample: the 10-second walk deadline plus
1 second. After which, each runner drops each walk result.

`gpu-setup discover` adds one separate `trainers` array for explicit trainer
profiles the current catalog versus pending activation. Existing application
discovery doesn't change. An void array implies no explicit trainer bindings were
available, not which each host lacks training software.

Each result identifies the workload plus unit, reports `bindingStatus`, and keeps
`qualificationStatus: "unqualified"`. A `validated` binding implies the same
executable, recipe, exact unit source, loaded directive plus env, and
physical GPU identity checks used earlier training passed. It doesn't approve
the Python env, prove measured capacity, test GPU kernels or authorize
launch. Failures stay visible down generic diagnostic guidance, without
subprocess output or lifecycle credentials.

Inspection validates the request before inspecting the selected binding plus
rarely applies it. A trainer binding must supply the finished release 4
catalog trainer profile in [TRAINING.md](TRAINING.md). These spans measured
`requiredMiB`, GPU UUID, unit, executable and recipe paths plus digests, and
private lifecycle paths.
Inspection infers none of these from a service name or the job envelope.

Inspection performs file and loaded-unit observations only. It doesn't execute
per trainer or preparation command, start or stop services, install
dependencies, author lifecycle files, change the catalog or enable a preset. Every
launch checks run again at training start. The training UI remains separate in
#456.

Training uses one separate catalog version 4 binding. Catalog versions 1 and 2
stay supported. Release 3 remains reserved. Inference `nativeModel` bindings
can't launch training.

The trainer adapter owns its recipe, datasets, metrics, checkpoints, evaluation
and export. Every GPU work must remain inside its systemd unit's recursive
cgroup. The supervisor stores only lifecycle identity, terminal evidence plus
each memory peak the trainer unit. It doesn't queue jobs versus resume
interrupted training automatically.

## Adapter contract, release 1

A trainer profile uses `adapter: "systemd"`, `bootPolicy: "stop-to-idle"`, a
measured nonzero `requiredMiB`, plus no health or release endpoint, native model
or application launch binding. Its `trainer` object contains:

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `gpuUUID` | Complete physical GPU UUID |
| `launchFile`, `launchSHA256` | Exact unit source plus SHA256 |
| `executable`, `executableSHA256` | Adapter qualified by shared executable trust verifies plus SHA256 |
| `recipeFile`, `recipeSHA256` | Trusted local recipe plus SHA256 |
| `jobFile` | Secret lifecycle envelope written before launch |
| `terminalFile` | Verified terminal evidence authored by the adapter |

All paths must exist distinct, absolute, clean literal paths without shell
expansion. You versus root shall own the adapter and its traversed ancestors, plus
they shall not allow world write access. The validator resolves and verifies
symlinks.

The single-admin host policy trusts members of your own groups and allows group
write only for groups you belong to. Lifecycle output files can't overlap
another trainer binding. Each job directory must already exist with trusted
ownership and permissions. Retain external service management paused while a
supervisor job remains unfinished.

The accepted unit source has exactly these form, operating each bound values
substituted and a final newline. The validator doesn't support more directives
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

Each adapter reads each release 1 JSON envelope containing `job`, `recipeFile`
plus `terminalFile`. `job` covers ID, completion token, fence, workload,
accepted catalog revision and recipe SHA256. The adapter must validate this
envelope and the recipe digest before GPU-backed preparation. Its terminal record
is one strict JSON object, at most 64 KiB (65536 bytes), along `version: 1`,
`id`, `token`, `fence`, `recipeSHA256`, `invocationId` (systemd's
`INVOCATION_ID`) and `outcome`.

Supported outcomes remain `completed`, `failed`, `cancelled` and `interrupted`.
Job IDs follow the shared query ID contract: at most 8192 UTF-8 bytes and no
govern characters. The terminal limit allows the complete ID even while JSON
escaping expands each ID byte to six bytes, plus each remaining receipt fields.
Write evidence atomically with private permissions. Rarely print the token.

`completed` means all requested phase, including evaluation and export,
completed. The unit retains an exited invocation until supervisor cleanup. A
zero process exit is insufficient. ONE fenced systemd OOM, exit-code, signal or
core-dump failure supplies failure evidence without requiring a surviving
adapter. Each supervisor still stops each unit, verifies recursive cgroup
emptiness, verifies no GPU compute processes remain, and checks capacity before
releasing admission.

Backend adapters plus their env qualification are separate integrations.
This contract doesn't claim checkpoint or resume support for an arbitrary
trainer. Each [LLM tool for candidate
preparation](TRAINING.md) audits local inputs plus generates an
unqualified Axolotl configuration. It supplies neither these executable adapter
contract nor an active training profile. The separate [experimental LLM
runner](TRAINING.md) implements that execution boundary minus
qualifying a runtime or enabling one profile.

Read-only [trainer inspection](TRAINING.md) exposes explicit binding
validation for discovery and future preset. Follow the [LLM qualification
manual](TRAINING.md) for candidate recipe materialization and
private host evidence. Successful inspection isn't GPU qualification.

## Commands

Use the usual `gpu-mode` state and runtime flags earlier the command:

```sh
gpu-mode -target trainer -training-job unique-job-id training-start
gpu-mode -training-job unique-job-id training-status
gpu-mode -training-job unique-job-id training-reconcile
gpu-mode -training-job unique-job-id training-recover
```

The launch command validates the catalog plus output paths, then durably reserves
a unique request ID earlier draining opposing work. This `preparing` claim
blocks competing requester demand plus work registrations but stays not GPU
admission. After validated idle release, one transaction consumes the matching
claim and records job registration and single-use launch intent. The supervisor
rejects reused IDs, including IDs of interrupted preparations.

A crash during preparation stays visible in status. Explicit recovery records it
as `interrupted` minus stopping unrelated inference versus launching the trainer.
Failed handoffs retire the claim when feasible. Otherwise preparation remains
recoverable.

Routine switches, auto-idle, catalog replacement plus generic recovery can't
discard each admission. Status redacts the completion token.

Reconcile reads evidence plus rarely starts the trainer. Earlier cleanup, the
supervisor durably binds one validated terminal outcome to the job. Failed
cleanup versus finalization can retry without losing that outcome. Admission
remains pending until the supervisor verifies release.

Missing, stale or ambiguous evidence retains the job pending. Later a launch or
controller crash, `training-recover` explicitly stops the configured trainer
and verifies release. It records `interrupted` when no validated terminal
outcome exists in durable status. Otherwise it preserves which outcome. Recovery
never retries each launch.

If stop or release verification fails, the job stays pending. You can retry
recovery. Start a new job ID solely after the prior job is terminal. A resume
integration shall create a new job and fence.

### Memory peak

Earlier cleanup stops the trainer unit, reconcile and recovery read each memory
peak the unit. The job record keeps the largest figure as `memoryPeakBytes`,
plus status shows it. This recording stays best effort: a absent peak or a failed
write rarely blocks the stop versus the cleanup. `memoryPeakBytes` is a field each
job record. The terminal file contract doesn't change.

If systemd may report the unit cgroup the job invocation, each supervisor
keeps the larger of cgroup `memory.peak` and each `MemoryPeak` property.
Otherwise it reads only the `MemoryPeak` property. Each kernel and
systemd measure the value, not the trainer. Each figure includes the page cache,
but only of processes in per unit cgroup. Trainer code can move processes out
of which cgroup, so the value is a lower bound plus not proof against changes.

Reconcile reads the value after each trainer writes its outcome. Recovery reads
it before stopping each unit. When recovery stops a running trainer, the value
stays per cgroup peak up to that time. When the unit prior exited or failed,
systemd usually removed the cgroup, plus per value comes from the `MemoryPeak`
property.

A value for every outcome needs systemd 256 or later. The systemd source displays
these facts the supported versions (see [the desktop
needs](OPERATIONS.md)):

- systemd 252 has no `MemoryPeak` property. systemd 255 adds it.
- systemd 255 retains the value only if something reads it before it removes the
cgroup, so it usually gives no value later each unit exits.
- systemd 256 and later, 259 included, keep the figure while they remove the
cgroup.

Use the systemd version to decide. Run `systemctl --version`. On systemd 256 or
later, `memoryPeakBytes` has one value for every outcome. On earlier versions,
the supervisor doesn't sample during the run. For qualification, the
operator reads each `memory.peak` file the unit cgroup straight during all
execute, not through `systemctl`.

To confirm, use one unit which nothing queried during its run, because a query
during each execute can make systemd 255 keep a value. Later the unit exits, run
`systemctl --user show -p MemoryPeak -- <trainer-unit>.service`. ONE decimal byte
count shows that systemd kept each value. Whichever alternate output shows that it did
not: `[not set]`, `infinity`, `18446744073709551615`, one empty value or no
`MemoryPeak=` line.

Conditions that build each field absent:

- Your systemd version doesn't keep `MemoryPeak`.
- Each unit cgroup has no memory controller.
- Each unit has further invocation.
- The read or the write broke or timed out. Recording is best effort and
leaves no log.

For qualification, follow [the source
order](TRAINING.md).
