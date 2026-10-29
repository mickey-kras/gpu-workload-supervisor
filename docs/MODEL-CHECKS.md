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

The model ID is 1 to 128 ASCII characters: letters, digits, `.`, `_` and `-`.
The first character is a letter or a digit.

## Inventory

The supervisor reads the top level of the model directory and calculates the
SHA-256 of each file. It uses the shared file digest and shard index rules of
`internal/llmtraining`:

- A model has at most 4096 files. A model with more files is `unsupported`.
- A symbolic link or a file that is not a regular file is a finding.
- A subdirectory is a finding. The only exception is the `.cache` directory of
  the download tool, see below.
- Each `*.safetensors.index.json` file is strict JSON of at most 128 MiB. It
  must have a `weight_map` that is not empty. Each entry must name a
  `.safetensors` file in the inventory. A larger index makes the model
  `unsupported`.

The `hf download` command with the `--local-dir` option writes a `.cache`
directory into the model directory. The inventory skips it, and the result
does not include it. No loader reads it. The `.cache` directory can hold only
these items:

- `.cache/huggingface/.gitignore`.
- Regular `*.metadata` and `*.lock` files in `.cache/huggingface/download`.

Any other item in `.cache` is a finding.

## Checks

The checks run in this order. Each must pass. A check that does not pass stops
the checks after it. Each check reads the files in name order, so the same
files give the same result.

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
- Files that no loader reads: `README`, `README.md`, `LICENSE`, `LICENSE.md`
  and `.gitattributes`.

The check rejects code files (`*.py`) and pickle formats (`.bin`, `.pt`,
`.pth`, `.pkl` and `.ckpt`). It rejects `adapter_config.json`, because a
loader then loads a base model that the checks did not see. It rejects all
other files.

The check reads each JSON file, at most 128 MiB. It rejects a file that is not
one strict JSON value and a duplicate key. It also rejects the keys
`auto_map`, `auto_mapping` and `custom_pipelines` at any depth. These keys
make a loader import code.

A model without `.safetensors` weights or without `config.json` is
`unsupported`.

### Upstream identity

The supervisor reads the file tree of the source at the pinned commit from the
Hugging Face API. Each local file must be in that tree:

- For an LFS file, such as weights, the SHA-256 in the LFS pointer must be
  equal to the local SHA-256.
- For another file, the local size must be equal to the upstream size. Then
  the git blob ID (SHA-1) of the local bytes must be equal to the upstream git
  blob ID. The source publishes no other digest for these files.

The upstream tree can have more files than the local directory.

The supervisor reads at most 64 pages of at most 16 MiB each, over HTTPS. It
does not follow redirects. It follows only next-page links to the same tree.
The check supports only public repositories now. A failed listing gives
`error` with one of these reasons: `upstream-not-found`, `upstream-denied`,
`upstream-unreachable` or `upstream-invalid`.

## Results

| Status | Cause |
| --- | --- |
| `passed` | All checks passed. Only this status is clean |
| `findings` | A check found a problem in the files, for example a rejected file, a remote code key, invalid JSON or an identity mismatch |
| `error` | A check did not complete, for example because of an unreadable file, a failed upstream listing or a file that changed during the check |
| `unsupported` | The checks do not support the model, for example a model without `.safetensors` weights |
| `not_scanned` | A check did not run, the files changed after the check, the checker changed, or there is no result |

The supervisor keeps one result for each model in its private state database,
as bounded strict JSON of at most 4 MiB. A result holds these items:

- The model ID, the directory and the source.
- The SHA-256 of each file.
- The status, the coverage, and the outcome and reason code of each check.
  Coverage `complete` means that all checks ran. Coverage `partial` means
  that one or more checks did not run.
- The checker version and the configuration: the hub URL and the file limits.
- The check time.

Reason codes never hold paths or file content.

The supervisor calculates the status again from the files on disk. If a file
digest, the directory, the source or the checker version differs from the
result, the status is `not_scanned`. Thus a new checker version checks each
model again.

A canceled check stores no result. The model stays `not_scanned`.

No signatures or keys protect the results. The deployment trusts processes
with the service UID (see [SECURITY.md](../SECURITY.md)). A process with that
UID can forge a result. This is an accepted limit.
