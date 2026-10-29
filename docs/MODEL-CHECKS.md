# Model checks

[Documentation](README.md) | [Architecture](ARCHITECTURE.md)

The supervisor checks each LLM before use
([#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585)).
Only a model with the status `passed` is clean. These checks are a defensive
layer. They do not prove that a model is safe. A clean result does not mean
that a modified model behaves like its original.

This page describes the checks that exist now. The chat template check, the
behavioral scan, the scan job, model selection in the desktop UI and the gate
of [#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583)
are not available yet. No command runs the checks yet. Thus no model can get
`passed` yet. A model that passes the available checks stays `not_scanned`.

## Model and source

A model is one local directory and its upstream source:

- The source is a Hugging Face repository, such as `org/name`, and a pinned
  commit of 40 lowercase hex characters. A branch or a tag is not a pin.
- Another directory, repository or commit is another model. A converted,
  quantized, merged or otherwise modified model is another model too. Results
  never carry over.

The model ID is 1 to 128 characters: letters, digits, `.`, `_` and `-`.

## Inventory

The supervisor walks the model directory with the shared training inventory
rules of `internal/llmtraining`. It calculates the SHA-256 of each file:

- A symbolic link or a file that is not a regular file stops the check.
- A model has 1 to 4096 files.
- Each `*.safetensors.index.json` file must match its inventory digest. Each
  `weight_map` entry must name an inventoried `.safetensors` file.

## Checks

The checks run in this order. Each must pass. A check that does not pass stops
the checks after it.

### File allowlist

All files must be directly in the model directory. The allowlist accepts only
these files:

- Weights: `*.safetensors` and `*.safetensors.index.json`.
- Configuration: `config.json`, `generation_config.json`,
  `hf_quant_config.json`, `preprocessor_config.json`, `processor_config.json`
  and `video_preprocessor_config.json`.
- Tokenizer: `tokenizer.json`, `tokenizer_config.json`, `tokenizer.model`,
  `special_tokens_map.json`, `added_tokens.json`, `vocab.json`, `vocab.txt`
  and `merges.txt`.
- Chat template: `chat_template.jinja` and `chat_template.json`.
- Adapter configuration: `adapter_config.json`.

The check rejects code files (`*.py`) and pickle formats (`.bin`, `.pt`,
`.pth`, `.pkl` and `.ckpt`). It rejects all other files, such as `README.md`,
and files in subdirectories. Download only the allowed files, for example with
the `--include` option of the Hugging Face download command.

The check reads each JSON file, at most 128 MiB. It rejects a file that is not
one strict JSON value, a duplicate key and an `auto_map` key at any depth. An
`auto_map` key makes a loader import code.

A model without `.safetensors` weights or without `config.json` is
`unsupported`.

### Upstream identity

The supervisor reads the file tree of the source at the pinned commit from the
Hugging Face API. Each local file must be in that tree:

- For an LFS file, such as weights, the SHA-256 in the LFS pointer must be
  equal to the local SHA-256.
- For another file, the git blob ID (SHA-1) must be equal to the git blob ID
  of the local bytes. The source publishes no other digest for these files.

The upstream tree can have more files than the local directory.

The supervisor reads at most 64 pages of at most 16 MiB each, over HTTPS. It
follows only next-page links to the same tree. Public repositories only. A
failed or invalid listing gives `error`.

## Results

| Status | Cause |
| --- | --- |
| `passed` | All checks passed. Only this status is clean |
| `findings` | A check found a problem: a rejected file, `auto_map`, invalid JSON or an identity mismatch |
| `error` | A check failed, the upstream listing failed or a file changed during the check |
| `unsupported` | The model has no `.safetensors` weights or no `config.json` |
| `not_scanned` | A check did not run, the files changed after the check, or there is no result |

The supervisor keeps one result for each model in its private state database,
as bounded strict JSON of at most 4 MiB. A result holds the model ID, the
directory, the source, the SHA-256 of each file, the outcome and reason code of
each check, the checker version and the check time. Reason codes never hold
paths or file content.

The supervisor calculates the status again from the files on disk. If a file
digest, the directory or the source differs from the result, the status is
`not_scanned`.

A cancelled check stores no result. The model stays `not_scanned`.

No signatures or keys protect the results. The supervisor and you share one
trusted UID (see [SECURITY.md](../SECURITY.md)). A process with that UID can
forge a result. This is an accepted limit.
