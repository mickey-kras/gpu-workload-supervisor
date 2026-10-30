# Training

Training presets stay disabled until [GPU qualification](#llm-training-qualification) passes. Preparation, discovery, dependency checks and repository tests do not approve runtimes. #453 stays open. Checkpoint and resume: #454. Training UI: #456.

- [Bind a trainer](#finite-training-jobs) and [start or recover a job](#commands).
- [Prepare inputs](#llm-training-candidate-preparation), [materialize a recipe](#materialize-and-inspect), and [inspect the binding](#trainer-binding-inspection).
- [Qualify the environment](#environment-and-backend-decision), [run training](#experimental-llm-training-runner), and [prepare an import](#llm-artifact-import-preparation).
- [Collect host evidence](#private-host-inputs-and-evidence), [supervisor memory peaks](#memory-peak), and [runner diagnostics](#resource-peaks).

## Finite training jobs

Training needs a separate version 4 catalog binding. Versions 1/2 remain supported. Version 3 stays reserved. Do not use inference `nativeModel` bindings for training.

The adapter owns recipes, datasets, metrics, checkpoints, evaluation and export. Keep GPU work in its unit's recursive cgroup. The supervisor stores lifecycle identity, terminal evidence and unit memory peak, without job queues or automatic resume.

### Adapter contract, version 1

Use `adapter: "systemd"`, `bootPolicy: "stop-to-idle"` and measured nonzero `requiredMiB`. Exclude health or release endpoints, native models and application launch bindings. The `trainer` object contains:

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `gpuUUID` | Full physical GPU UUID |
| `launchFile`, `launchSHA256` | Exact unit source and SHA256 |
| `executable`, `executableSHA256` | Adapter qualified by shared executable trust checks and SHA256 |
| `recipeFile`, `recipeSHA256` | Trusted local recipe and SHA256 |
| `jobFile` | Private lifecycle envelope written before launch |
| `terminalFile` | Trusted terminal evidence written by the adapter |

Use distinct absolute literal paths without shell expansion. The validator resolves symlinks and checks ownership of the adapter and ancestors (root or user). It rejects world write and permits group write only for your groups. Lifecycle outputs cannot overlap trainer bindings. Create the trusted job directory first. Pause external service management while the supervisor job remains unfinished.

Substitute bound values in this exact unit source, including its final newline. The validator rejects more directives and drop-ins:

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

Before GPU preparation, validate the recipe digest and version 1 envelope: `job`, `recipeFile`, `terminalFile`. `job` contains ID, token, fence, workload, accepted catalog revision and recipe SHA256.

Publish private terminal evidence atomically. Never print tokens. Strict JSON, at most 64 KiB (65536 bytes): `version: 1`, `id`, `token`, `fence`, `recipeSHA256`, `invocationId` (systemd `INVOCATION_ID`), `outcome`. Outcomes: `completed`, `failed`, `cancelled`, `interrupted`. IDs: at most 8192 UTF-8 bytes, no controls. The limit accommodates six-byte JSON escaping per ID byte plus receipt fields.

`completed` means all requested phases, including evaluation and export, finished. Exit zero alone is insufficient. Keep the exited invocation until cleanup. Fenced systemd OOM, exit-code, signal or core-dump failures supply evidence without surviving adapters. Release the admission only after stopping, empty recursive cgroup, absent GPU compute processes and capacity checks.

Qualify each backend and environment separately. This contract promises no arbitrary-trainer checkpoint and resume support. Candidate preparation supplies no executable adapter or enabled profile. The experimental runner supplies execution, not qualification.

### Commands

Put the usual state and runtime flags before each command:

```sh
gpu-mode -target trainer -training-job unique-job-id training-start
gpu-mode -training-job unique-job-id training-status
gpu-mode -training-job unique-job-id training-reconcile
gpu-mode -training-job unique-job-id training-recover
```

| Operation | Admission and recovery rules |
| --- | --- |
| Start | Validate catalog and output paths. Durably reserve a unique ID before draining opposing work. The `preparing` claim blocks competing requester demand and work registrations, but grants no GPU admission. After verified idle release, one transaction consumes the matching claim and records registration plus single-use launch intent. Never reuse IDs, including interrupted preparations. |
| Preparation failure | Status exposes crashes. Explicit recovery records `interrupted` without stopping unrelated inference or launching training. Failed handoffs retire the claim when possible. Otherwise preparation remains recoverable. |
| Active job | Routine switches, auto-idle, catalog replacement and generic recovery cannot discard admission. Status redacts the token. |
| Reconcile | Read evidence without starting training. Durably bind the validated terminal outcome before cleanup. Retry failed cleanup or finalization without losing it. Keep admission pending until verified release. |
| Recover | Missing, stale or ambiguous evidence leaves the job pending. After launch or controller crashes, `training-recover` explicitly stops the configured trainer and verifies release. Preserve any durable verified outcome. Otherwise record `interrupted`. Never retry launch. |
| Retry | Failed stop or release keeps the job pending. Retry recovery. Use a new ID only after the prior job becomes terminal. Resume integrations need a new job and fence. |

### Memory peak

Before cleanup, reconcile and recovery record the largest `memoryPeakBytes` in job status without changing terminal fields. Missing memory controllers, different invocations and failed or timed-out reads or writes can leave it absent. Recording is best effort: no blocked cleanup or logs.

For the same invocation, record max(cgroup `memory.peak`, systemd `MemoryPeak`), or only `MemoryPeak` without a cgroup. The kernel and systemd measurement includes cgroup page cache. Processes can escape the cgroup: the value is a lower bound, not tamper-proof evidence.

Reconcile samples after terminal evidence, recovery before stopping. Running-unit recovery measures up to that time. Exited or failed units usually lack cgroups and use `MemoryPeak`.

| systemd | Availability after exit |
| --- | --- |
| 252 | No `MemoryPeak` property. |
| 255 | Keeps the value only if something reads it before cgroup removal. Usually absent after exit. |
| 256 and later, including 259 | Keeps the value across cgroup removal for every outcome. |

Check `systemctl --version`. Before 256, operators must sample unit `memory.peak` directly each qualification run. The supervisor does not sample during execution.

Test an unqueried unit after exit: `systemctl --user show -p MemoryPeak -- <trainer-unit>.service`. Decimal bytes prove retention. `[not set]`, `infinity`, `18446744073709551615`, empty output and absent `MemoryPeak=` do not. See [evidence order](#private-host-inputs-and-evidence) and [desktop versions](OPERATIONS.md#local-gnome-desktop-installation).

## LLM training candidate preparation

The release archives and Debian packages include `gpu-training-candidate`. Preparation reads local inputs without network, runtime imports (including version and plugin discovery), subprocesses or dataset logging. Errors omit values and contents. It launches, enables and approves nothing and supplies no measured GPU or storage capacity or serving qualification.

```sh
gpu-training-candidate -recipe /private/candidate.json -output /private/new-plan.json
```

Alternatively, use the finite-job envelope and current `INVOCATION_ID`:

```sh
gpu-training-candidate -job-file /private/training/job.json -output /private/new-plan.json
```

Job mode verifies launch identity and raw recipe SHA256, then repeats input and resource checks. Plans include identity and invocation, never tokens. Local checks do not prove that the invocation is live or the fence is current. Plans cannot acknowledge completion or authorize GPU work.

Use a private output parent. The command writes and closes a private temporary file, then publishes a new `0600` file without replacement. Hard-link support is necessary. Failed writes leave no final plan to block retry.

Recipe plans: `status: "unqualified"`, row counts, estimates, `canonicalRecipeSHA256`, `config`. With `-job-file` instead of `-recipe`, these nest under `candidate` (including `candidate.config`, `candidate.canonicalRecipeSHA256`). Outer `recipeSHA256` hashes original bytes. Canonical hashes use Go's typed JSON encoding, including inventories and dataset digests. Keep the original recipe.

Only `config` is JSON-subset YAML for the [inspected source](#inspected-upstream-api), not the whole plan. Compilation proves no model-loading compatibility.

### Recipe version 1

Strict JSON, at most 8 MiB. Reject unknown or duplicate keys, trailing values, remote paths and arbitrary trainer or configuration overrides. Full typed schema: `internal/llmtraining/recipe.go`.

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

| Input | Validation and limits |
| --- | --- |
| Inventory | Reject missing, extra, linked or nonregular files. At most 65536 enumerated entries, including empty directories but not root. At most 32768 declared files. Directory reads stop at remaining budget plus one overflow entry. Output emptiness reads one entry. |
| Provenance | Inventory equality proves preparation-time bytes only, not revision origin, code or entry-point approval, external libraries or later immutability. Include runtime distribution metadata and entry-point declarations. |
| Model and tokenizer | Only `model_type: "llama"`. Local `config.json`, at least one `.safetensors`, separate `tokenizer_config.json`. Asset extensions: `.json`, `.safetensors`, `.model`, `.txt`. Reject `auto_map` and model `quantization_config`. No GGUF, pickle, prequantized inputs or custom model code. Tensor shapes, tokenizer compatibility and model loading need qualification. |
| Datasets | UTF-8 JSONL with only `{"text":"..."}` records. Per split: 64 MiB, lines less than 64 KiB, at most 50000 records. Reject empty text and exact decoded-text overlap between splits. This is not semantic deduplication. |

Tokenization can split long completion records. Row counts are not token counts. Keep datasets and artifacts outside public history.

### Resource estimates

Supply nonzero `ramBytes`, `weightsBytes`, `cacheBytes`, `checkpointBytes`, `exportBytes`, `reserveBytes`. `retainedCheckpoints` is 1-8. Overflow-checked storage:

```
weights + cache + export + reserve + checkpoint * (retainedCheckpoints + 1)
```

The estimate includes existing weights and one in-progress save. Check the output-filesystem free space and Linux free RAM excluding reclaimable cache. Cache and training output share the output directory. Checks reserve nothing, predict no training use or GPU compatibility, and do not measure capacity.

### Candidate configuration

| Configuration | Value |
| --- | --- |
| Common | Full-text completion, explicit held-out data, no packing, BF16, SDPA, gradient checkpointing, AdamW, constant learning rate, LoRA `q_proj`/`v_proj`. |
| Quantization | QLoRA: NF4 with double quantization. LoRA: neither 4-bit nor 8-bit loading. |
| Evaluation and save | Final step. Upstream also evaluates before training. |
| Resume | Off. `save_only_model: false` requests optimizer and training state, not proof of checkpoint-stop and resume correctness. |

Disable reporting and Hub uploads. This neither contains network access nor approves auto-loaded plugins or model-support entry points, even with empty `plugins`. See [presets](#immutable-asset-candidates) and [runner isolation](#experimental-llm-training-runner).

### Qualification still required

Before backend selection, compare Axolotl, TRL with PEFT and Unsloth with [equal settings](#environment-and-backend-decision). Freeze the runtime bytes and entry points. Qualify Torch, CUDA, kernel and GPU compatibility and privately measure RAM, VRAM and storage. Preparation, execution, export identity, serving compatibility and activation and checkpoint and resume remain separate.

### Inspected upstream API

Configuration mapping targets the Axolotl source `1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`, not an approved environment lock:

- [Local JSON and YAML loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/cli/config.py)
- [Local dataset loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/utils/data/shared.py)
- [Completion text strategy](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/prompt_strategies/completion.py)
- [Model and NF4 loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/loaders/model.py)
- [Reporting and upload configuration](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/core/builders/base.py)

## LLM training qualification

Keep host identities, paths, datasets, generated text and raw evidence private. Qualification needs every gate below. Missing evidence, unsupported combinations, unmeasured peaks or failures keep #453 open.

### Environment and backend decision

[Dependency input](../training/axolotl/requirements.in) supplies the local SFT and LoRA worker subset, Torch 2.14.0 and build tools. It omits Gradio, Trackio, s3fs, gcsfs, adlfs, ocifs, lm_eval and openenv-core, and overrides python-dotenv. Floors: fsspec >=2026.6.0, Pillow >=13, python-dotenv >=1.2.2. CI permits exactly these nine deviations (eight absent distributions, one override).

The worker rejects excluded distributions. Axolotl source and installed dependency metadata stay unchanged. No fork.

Hash lock: Linux x86_64, CPython 3.12.14. This is a dependency candidate, not environment approval. Source hashes do not pin wheels: record wheel hashes and build tools and test a private wheelhouse offline. Never install into inference environments.

For a dedicated environment, use an absolute `PYTHON` path and install the lock before unchanged Axolotl source:

```sh
uv pip sync --python "${PYTHON:?}" --require-hashes training/axolotl/requirements.txt
uv pip install --python "$PYTHON" --no-deps --no-build-isolation \
  'axolotl @ git+https://github.com/axolotl-ai-cloud/axolotl.git@1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1'
"$PYTHON" -I -S -B training/axolotl/check_environment.py
sudo unshare --net -- timeout 120 "$PYTHON" training/axolotl/smoke.py
```

Installation needs pinned-artifact access. CPU smoke exercises actual setup, train, evaluate and save, tiny random weights, two LoRA optimizer steps and separate-process reload with finite loss and logits. Its offline namespace and 120-second limit match CI. This tests dependency updates, not GPU suitability or production CUDA reload and generation.

`pip check` reports nine intentional deviations. Use the exact checker. Never ignore other failures or edit installed metadata.

Regenerate only for explicit dependency updates with uv 0.12.23:

```sh
uv pip compile training/axolotl/requirements.in \
  --python-version 3.12.14 --python-platform x86_64-unknown-linux-gnu \
  --generate-hashes --no-annotate --no-header \
  --output-file training/axolotl/requirements.txt
```

Install the noneditable Axolotl 0.21.0 at the pinned commit with `--no-deps`. Preserve genuine VCS `direct_url.json`. Inventoried regular `bin/python3` and `lib/pythonX.Y/site-packages` are necessary, not a symlinked virtualenv. Separately record interpreter, stdlib and shared-library hashes and provenance.

Source review dated 2026-10-22 identifies constraints, not a performance winner:

| Candidate source | Verified integration constraint | Remaining evidence |
| --- | --- | --- |
| [Axolotl 1f1eb05](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/pyproject.toml) | Python >=3.12. Transformers 5.20.1, Datasets 5.0.1, PEFT 0.21.2, TRL 1.14.1. Existing supervised worker targets this source | Complete installed closure, both recipes, resource measurements |
| [TRL 1.14.1 fd74bbc](https://github.com/huggingface/trl/blob/fd74bbc7b5f852a70d4cc94377e0a8f94392fda1/pyproject.toml) with PEFT | Python >=3.10. Datasets >=4.7.0 and Transformers >=4.56.2. Independent reference adapter still needs qualification | Equal tokenization, loss, optimizer steps, export and fresh reload |
| [Unsloth 356721b](https://github.com/unslothai/unsloth/blob/356721b31ebef3c1c0fcb8d53572c5be3d6eba89/pyproject.toml) | Its training extra caps Transformers at 5.17.0 and Datasets below 5.0.0. Cannot share this Axolotl environment | Separate resolved closure, equal recipe and measured comparison |

Use separate pinned environments. Match assets, dataset bytes and splits, seed, LoRA targets, rank, alpha and dropout. Match BF16, sequence length, batch and accumulation, optimizer, learning rate and steps. Disable packing, reporting and uploads.

Record the tokenization differences. Equal row counts do not prove equal work. Do not bypass constraints for shared environments. The runner supports only Axolotl.

#### Runtime check

Run `"$PYTHON" -I -S -B training/axolotl/check_environment.py` only in dedicated training environments. `-I`/`-S` are mandatory. No bytecode even without `-B`. Checker site-packages follow stdlib. It verifies compatibility, not provenance, GPU kernels or changed or damaged bytes.

Exit 0 means `accepted`. 1 means `rejected`. Exit 2 means either an internal error (same JSON keys, rejected status, empty `checks`, `error` text) or argparse usage on stderr without JSON.

| Target | Rule |
| --- | --- |
| `cpu-smoke` (default) | Preset-only problems have the status `warn` and do not reject the CPU smoke |
| `smoke-lora` or `representative-qlora` | Each problem has the status `fail` and rejects the report |

Use `--target` to select the target. Preset targets need `--gpu-uuid` in supervisor format. The child sets `CUDA_VISIBLE_DEVICES` accordingly. Missing UUID fails `accelerator`.

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

Stdlib names include `sys.stdlib_module_names` and private names such as `_sysconfigdata_*`. Pre-child and post-child scans reject changed shadowing or startup-file findings, not changed contents. Runner and checker `-S` skips `.pth`, but compile workers can process it. Accept only `.pth` listed in locked-wheel `RECORD`.

Native, driver and accelerator probes use worker-style site-packages first, `-I -S -B`, and sanitized runner variables. Drop ambient `LD_LIBRARY_PATH`, `LD_PRELOAD`, `BNB_CUDA_VERSION`. Stop the child after 300 seconds.

Every target, including CPU smoke, rejects unlocked distributions. Use `uv pip sync --require-hashes`, not `uv pip install`, for the lock. Installation checks integrity. Runtime checks detect version or wheel-tag drift, not damage.

The lock includes xformers linked to `libcudart.so.12`, but no such library: Torch uses CUDA 13.0. Presets use SDPA. The checker does not load xformers.

CUDA requires visible GPU, CUDA Torch, driver support for at least Torch's CUDA major, compatible kernels and native BF16 (capability >=8.0).

SASS targets need the same major and no higher minor than the GPU. PTX needs equal or higher GPU capability and a driver for the Torch CUDA version or newer. `a` targets such as `sm_90a` need exact capability. `f` targets such as `sm_100f` need the same major. QLoRA also needs bitsandbytes.

Without usable GPU, the report shows `accelerator: "cpu"`. Every worker phase rechecks and never falls back to CPU training. Reports explain faults and corrections without paths, credentials or dataset contents.

### Immutable asset candidates

[Asset manifest](../training/assets.json) records real source revisions and unqualified parameter sets:

| Preset | Model and tokenizer snapshot | Parameters |
| --- | --- | --- |
| `smoke-lora` | [TinyLlama 1.1B](https://huggingface.co/TinyLlama/TinyLlama-1.1B-intermediate-step-1431k-3T/tree/59f6f375b26bde864a6ca194a9a3044570490064) | LoRA 8/16, sequence 128, batch 1, accumulation 1, 2 steps |
| `representative-qlora` | [Llama 3.1 8B, NousResearch distribution](https://huggingface.co/NousResearch/Meta-Llama-3.1-8B/tree/1f47e50cdbe801ad8a5174156ec3a0655108fb9f) | QLoRA 16/32, sequence 2048, batch 1, accumulation 4, 100 steps |

Both presets use learning rate 0.0002 and seed 42. These training weights are not GGUF. Source pins prove neither fit nor quality. `assets.json` guides qualification, not materialization.

Presets fix parameters. Materialization accepts supplied full revisions and hashes local inventory bytes. Compare both revisions against the table. No placeholder revisions, fake inventories or memory claims.

Follow model licenses. Download model and tokenizer into separate new directories through installed-client [snapshot_download](https://huggingface.co/docs/huggingface_hub/guides/download), with exact `revision` and `local_dir`. Allow model `config.json`, `generation_config.json`, `model*.safetensors`, shard-index JSON. Tokenizer `tokenizer.json`, `tokenizer.model`, `tokenizer_config.json`, `special_tokens_map.json`, `added_tokens.json`.

Exclude pickle and code.

Before inventorying, remove only downloader `.cache/huggingface` metadata. Downloads and installations are explicit deployment steps outside activation.

### Materialize and inspect

Use a private input object with exactly these fields:

| Field | Value |
| --- | --- |
| `version`, `preset` | `1`. `smoke-lora` or `representative-qlora` |
| `model`, `tokenizer` | Each has `root` (absolute local directory) and `revision` (the full snapshot SHA) |
| `runtimeRoot` | Absolute dedicated runtime directory |
| `train`, `heldOut` | Absolute, distinct local JSONL paths using the candidate text schema |
| `outputDirectory` | New, empty, private absolute directory |
| `budget` | All fields from [candidate resource estimates](TRAINING.md#resource-estimates). Explicit operator estimates, never zeros or inferred from model size |

Use absolute paths and private parents. Recipe and plan destinations must be new. Each run needs distinct output directory, recipe and job ID.

```sh
gpu-training-candidate -version
gpu-training-runner -version
gpu-training-candidate -materialize "${INPUTS:?}" -output "${RECIPE:?}"
gpu-training-candidate -recipe "$RECIPE" -output "${PLAN:?}"
sha256sum "$RECIPE" "$PLAN"
```

Materialization inventories bytes and validates candidates without source authentication, downloads, installation or execution. Keep inputs and recipes private. Trainer binding also needs unit, executable and recipe hashes, physical UUID, cgroup, distinct lifecycle paths and measured `requiredMiB`. Inspection invents no measurements or environment approval.

### Private host inputs and evidence

Collect these read-only observations privately before selecting a deployment lock, never in public issues/CI logs:

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

Set `PRESET` and trainer `GPU_UUID`. Rejected reports block the preset: fix each `fail`, then repeat.

Also keep:

- Release and checksum, approved interpreter and version and stdlib inventory.
- Distribution metadata, wheel and source hashes, ELF loader and library identities.
- CUDA, Torch, architecture and driver identities and reviewed egress containment.
- Asset sources and inventories, dataset digests and counts, recipe and plan.
- Unit bytes and loaded properties, existing inference catalog and revision.

Exclude raw environment dumps and completion tokens.

After qualification and measured binding, use the supervisor, never worker or unit directly. Add usual state and runtime flags. Keep outputs private:

```sh
gpu-mode -target "${TRAINER_PROFILE:?}" -training-job "${JOB_ID:?}" training-start
gpu-mode -training-job "$JOB_ID" training-status
gpu-mode -training-job "$JOB_ID" training-reconcile
gpu-mode -training-job "$JOB_ID" training-status
```

Reconcile after trainer exit. Invalid or missing evidence leaves admission pending. For interruption or uncertain launch, use `gpu-mode -training-job "$JOB_ID" training-recover`. It stops and verifies release, never retries or resumes.

For each backend and model size, keep:

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

Sample `nvidia-smi` throughout. Record the interval. Sampled VRAM is a lower bound. For RAM, prefer [`memoryPeakBytes`](#memory-peak), then direct unit `memory.peak` samples. [`peaks.json`](#resource-peaks) is an unverified hint.

Measure the disk during saves, not just after pruning. Add capacity headroom. Close the qualification only with a reviewed record covering every real-GPU gate.

## Experimental LLM training runner

Invoke `gpu-training-runner --job-file /private/training/job.json` only through the exact bound supervisor unit, physical GPU UUID and systemd invocation. It implements the finite-job adapter and strict candidate recipe, without enabled presets, serving compatibility or checkpoint and resume support.

Use the strict recipe and [runtime layout](#environment-and-backend-decision), Python >=3.12, complete input inventories and digests, resource preflight and empty private output. Runtime ownership: root or execution user, no group or other write. `-I -S -B` excludes ambient paths, user site, startup `.pth` and bytecode. The worker adds inventoried site-packages. `direct_url.json` must identify pinned noneditable Axolotl.

Before Axolotl import, check entry-point owner, group, name and value. Allow only its two bundled context and expert-parallel plugins and four cloud providers: CoreWeave, Nebius, Modal, Baseten. Inspect the metadata without importing or using providers. Reject other plugins, providers or model entry points. Metadata permission does not authorize cloud or parallel training.

Each phase runs the accelerator check. `load_in_4bit` must be boolean. Unsupported combinations stop execution.

Compatibility checks do not pin interpreter, stdlib, OS, native libraries, CUDA or driver provenance or approve the environment.

Create a fresh private `.llm-<invocation>` beside the envelope. Reuse fails. It holds config, embedded worker, isolated home and cache, bounded current progress, phase receipts and best-effort peaks.

Never expose completion credentials in worker files, arguments or environment.

Discard the child stdout and stderr. Errors name failed phases without paths, data or credentials. Keep private artifacts for diagnosis and use the runtime check for rejection reasons.

Axolotl setup and native Trainer APIs must reach the exact optimizer step, pass finite nonnegative held-out loss and save adapter and tokenizer. A separate process reloads base and adapter, checks finite forward output and bounded deterministic continuation. Both remain under the same unit and admission. No generated text enters evidence.

After both phases succeed, validate export, write version 2 `supervisor-provenance.json`, then atomically publish fenced `completed`. Provenance binds source, raw recipe, asset revisions and inventories, runtime inventory and entrypoint, worker, invocation, export hashes and held-out loss. Never tokens. Adapter, config and tokenizer files must remain unchanged across reload. `exportBytes` bounds their combined size.

Failed phases publish no success. Systemd or recovery handles failure or interruption. Supervisor release and capacity checks remain independent.

Enable Hub and dataset offline modes, omit ambient credentials, disable reporting and telemetry (`AXOLOTL_DO_NOT_TRACK=1`). These are not network isolation: qualify deployment restrictions. Fixtures test process, environment, receipt, failure and provenance. Python guard tests cover source and plugin rejection, atomic progress and mocked accelerator rules. They prove neither weight loading, GPU kernels nor real training and reload.

### Resource peaks

Treat `peaks.json` as unverified diagnostics: trainer-unit code can rewrite it. A failed job deliberately has no receipt. Terminal evidence, not diagnostic outcome, is authoritative.

The runner writes once after completed receipt or failure, only after creating `.llm-<invocation>`. Early validation failure, OOM kill or `SIGKILL` can prevent the file. Failed writes never change the outcome. Its single JSON object is at most a few hundred bytes, without paths, data or credentials.

One final sample includes the cgroup peak. Successful runs sample before final input checks, provenance and receipt. Earlier failures sample after failure. Failures during or after final checks keep the pre-check sample.

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

`null` means unmeasured. GPU samples occur per optimizer step, after evaluation, save and reload, using the Torch allocator peak of the worker. They exclude CUDA context and other nonallocator memory, so form a lower bound unaffected by other GPU processes. On CUDA OOM, try one last sample. Other errors make no CUDA call, killed workers none.

Failed samples never fail a phase.

Storage includes retained checkpoints, in-progress save, cache and export across output and execution directories. Sampling can miss peaks. Each walk stops at 65536 entries, 10 seconds, unreadable directories, phase end or stop signal. Incomplete walks produce no value. No completed walk means `null`.

Open the directories relative to parents without following links or crossing mounts. Replacement symlinks cannot redirect walks.

Blocked filesystem calls can hold a walk. Wait at most 1 second at phase end. For final sampling, wait at most 11 seconds (10+1), then discard the result.

## LLM artifact import preparation

`gpu-training-import` validates local PEFT identity against the original recipe and publishes a private plan. It does not copy or convert weights, register or replace serving models or activate workloads:

```sh
gpu-training-import -format peft -recipe /private/recipe.json -output /private/new-import-plan.json
```

Use a private parent and new output file. Plan: `status: "unqualified"`, `servingQualified: false`. `peft` denotes format, not runtime. Reject vLLM, llama.cpp, GGUF and other targets. Qualify serving version, base and adapter and supervised handoff separately. Reload alone proves no serving compatibility.

Use version 2 provenance from the same worker and source revision and supervisor release. Recheck model, tokenizer and runtime inventories, raw recipe digest and every export. Reject weight-only provenance. Added, removed or changed participating files block completion. Materialization and import fully read model weights for hashing.

| Export | Required contract |
| --- | --- |
| Required | One `adapter_model.safetensors`, `adapter_config.json`, `tokenizer_config.json`, and `tokenizer.json` or `tokenizer.model`. |
| Optional bound files | `special_tokens_map.json`, `added_tokens.json`, `generation_config.json`, `config.json`, `vocab.json`, `vocab.txt`, `merges.txt`, `chat_template.jinja`. |
| Excluded | Checkpoints, caches, training arguments, README and other outputs. No sharded or pickle adapters, template directories or redirects to other code or files. |
| Adapter | Declare LoRA, causal language modeling and the recipe's precise local base path. |

Only named files enter plans. Revalidate before copying or loading: snapshots are neither signatures nor protection from trusted-owner data and metadata rewrites. Keep paths and inventories private. Import executes no runtime, excludes datasets and tokens, and proves no finalization, GPU release, environment approval or serving fitness.

## Trainer binding inspection

`gpu-setup discover` adds separate `trainers` results for explicit catalog or pending-activation profiles without changing application discovery. Empty means no explicit bindings, not no training software.

Each result identifies workload and unit, `bindingStatus` and `qualificationStatus: "unqualified"`. `verified` means executable, recipe, exact unit source, loaded command and environment and physical GPU checks passed. It approves neither Python environment, measured capacity, kernels nor launch. Failures remain visible with generic guidance, without subprocess output or credentials.

Validate requests before selected-binding observation. Never apply. Use the complete version 4 profile, including measured `requiredMiB`, UUID, unit, executable and recipe paths and digests and private lifecycle paths. Infer nothing from service names or envelopes. Inspection only observes files and loaded units: no trainer or preparation execution, service changes, installations, lifecycle writes, catalog changes or preset enablement. Repeat all launch checks at training start.
