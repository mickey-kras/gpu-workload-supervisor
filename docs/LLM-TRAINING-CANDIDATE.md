# LLM training candidate preparation

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
The [qualification guide](LLM-TRAINING-QUALIFICATION.md#materialize-and-inspect)
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

## Recipe version 1

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

## Resource estimates

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

## Candidate configuration

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

## Qualification still required

Compare Axolotl, TRL with PEFT and Unsloth using the same pinned base and
tokenizer, datasets, split and training settings before selecting one supported
backend. Freeze all runtime bytes and entry points, including Torch, CUDA and
kernel and GPU architecture compatibility. Record the measured RAM, VRAM and
storage privately.

The [experimental runner](LLM-TRAINING-RUNNER.md) adds the supervised execution
boundary, sanitized child environment, bounded progress, held-out evaluation,
fresh-process reload and complete export provenance. It still needs a
completely pinned, qualified runtime and real model and GPU evidence. The [tool
for import preparation](LLM-TRAINING-IMPORT.md) validates artifact identity.
Serving compatibility and activation remain separate qualification steps. This
preparation tool still creates candidate material only. #453 remains
incomplete.

## Inspected upstream API

Configuration mapping targets Axolotl at source commit
`1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`, not a supported environment lock:

- [Local JSON and YAML loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/cli/config.py)
- [Local dataset loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/utils/data/shared.py)
- [Completion text strategy](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/prompt_strategies/completion.py)
- [Model and NF4 loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/loaders/model.py)
- [Reporting and upload configuration](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/core/builders/base.py)
