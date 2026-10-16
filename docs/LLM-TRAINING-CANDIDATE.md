# LLM training candidate preparation

This is a preparation-only increment for #453. It does not launch training,
enable a catalog profile, approve a runtime, or make an output eligible for
serving import. No GPU memory or storage measurements are published.

Build `./cmd/gpu-training-candidate`, then run it with two absolute paths:

```sh
gpu-training-candidate -recipe /private/candidate.json -output /private/new-plan.json
```

The command reads local inputs and creates one new file with mode `0600`.
It refuses to replace an existing file. Keep its parent directory private.
The plan is written to a private temporary file in that directory and published
only after writing and closing succeed. Publication requires hard-link support
on the destination filesystem; a failed write leaves no final plan to block retry.
There is no network access, runtime import, subprocess launch or dataset-content
logging in the preparation command. Errors omit input values and file contents.
The command is not installed by the release package.

The plan contains `status: "unqualified"`, input row counts, operator-supplied
resource estimates, a canonical recipe digest and a generated `config` object.
The plan itself is **not** an Axolotl configuration file. Its `config` object is
JSON-subset YAML targeting the inspected source revision below. A successful
compile does not demonstrate that Axolotl can load or train this model.

## Recipe version 1

Recipes are strict JSON, at most 8 MiB. Unknown keys, duplicate keys, trailing
values, remote paths and arbitrary trainer/configuration overrides are rejected.
The complete typed schema is `internal/llmtraining/recipe.go`.

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

Inventories reject missing, extra, linked or nonregular files. Inventory matching
is only a byte comparison at preparation time: it does not prove that bytes
originate from the declared revision, approve code or installed entry points,
bind external libraries, or prevent subsequent modification. Include installed
distribution metadata and entry-point declarations in the runtime inventory.
The runtime is never imported, even to discover its version or plugins.

The candidate is limited to `model_type: "llama"`, local `config.json`, at least
one `.safetensors` file, and a separate `tokenizer_config.json`. Asset inventories
permit only `.json`, `.safetensors`, `.model` and `.txt` files. Configurations with
`auto_map` or model `quantization_config` are rejected. GGUF, pickle weights,
prequantized input and custom model code are outside this candidate. File names
and configuration checks do not validate tensor shapes, tokenizer compatibility
or actual model loading; those remain qualification steps.

Each dataset is UTF-8 JSONL containing only `{"text":"..."}` records. Limits are
64 MiB per file, less than 64 KiB per line and 50000 records per split. Empty text
and exact decoded-text overlap between training and held-out splits are rejected.
This is not semantic deduplication. Tokenization is deferred; row counts are not
token counts. Axolotl's completion strategy can chunk a long record into multiple
sequences. Keep private dataset contents and resulting artifacts outside public
repository history.

## Resource estimates

Every byte estimate must be nonzero: `ramBytes`, `weightsBytes`, `cacheBytes`,
`checkpointBytes`, `exportBytes`, `reserveBytes`. `retainedCheckpoints` is 1-8.
The overflow-checked storage estimate is:

```
weights + cache + export + reserve + checkpoint * (retainedCheckpoints + 1)
```

The extra checkpoint accounts for an in-progress save. Preparation compares this
conservative total with available space on the output filesystem and compares
RAM with Linux free RAM, excluding reclaimable cache. It reserves neither RAM
nor disk and cannot predict training use. Existing weights are still included in
the total. Generated dataset cache and training output share the output directory.
These checks are estimates, not measured capacity or GPU compatibility checks.

## Candidate configuration

The fixed configuration uses full-text completion, explicit held-out data, no
sample packing, BF16, SDPA, gradient checkpointing, AdamW, a constant learning
rate, and LoRA targets `q_proj`/`v_proj`. QLoRA enables Axolotl's NF4 conversion
with double quantization; LoRA leaves 4-bit and 8-bit loading off. Evaluation and
checkpoint saving are requested at the final step; automatic resume is off.
Checkpoints request optimizer/training state (`save_only_model: false`), which
does not establish checkpoint-stop or resume correctness.

Reporting integrations and Hub uploads are disabled in generated configuration.
This alone is not network containment. A future runner must use a sanitized
environment, disable telemetry with `AXOLOTL_DO_NOT_TRACK=1`, and bind the complete
approved runtime. Axolotl may auto-load installed plugin/model-support entry
points even when `plugins` is empty. This tool does not approve those plugins.

`canonicalRecipeSHA256` hashes the typed recipe re-encoded with Go's JSON encoder,
including inventories and dataset digests. Preserve the original recipe with the
plan. This is not the raw recipe-file digest used by the finite-job envelope.

Suggested **unqualified parameter sets**, requiring real immutable local assets:

| Candidate | Adapter | Rank/alpha | Sequence | Microbatch/accumulation | Steps |
| --- | --- | --- | --- | --- | --- |
| Small Llama smoke model | LoRA | 8/16 | 128 | 1/1 | 2 |
| Representative 7-8B Llama | QLoRA | 16/32 | 2048 | 1/4 | 100 |

Both use learning rate 0.0002 and seed 42. No placeholder revision, fake inventory,
or claimed memory budget is provided as a runnable recipe.

## Qualification still required

Compare Axolotl, TRL/PEFT and Unsloth using the same pinned base/tokenizer,
datasets, split and training settings before selecting one supported backend.
Freeze all runtime bytes and entry points, including Torch, CUDA/kernel and GPU
architecture compatibility. Record measured RAM, VRAM and storage privately.

Then implement the supervised finite-job adapter, sanitized child environment,
bounded progress, held-out evaluation, fresh-process reload/inference check,
adapter provenance and explicit serving-import validation. Emit successful fenced
terminal evidence only after all required phases pass. Until then, this tool
creates preparation material only; #453 remains incomplete.

## Inspected upstream API

Configuration mapping targets Axolotl source commit
`1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`, not a supported environment lock:

- [Local JSON/YAML loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/cli/config.py)
- [Local dataset loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/utils/data/shared.py)
- [Completion text strategy](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/prompt_strategies/completion.py)
- [Model and NF4 loading](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/loaders/model.py)
- [Reporting and upload configuration](https://github.com/axolotl-ai-cloud/axolotl/blob/1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1/src/axolotl/core/builders/base.py)
