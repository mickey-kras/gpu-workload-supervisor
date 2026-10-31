# Model checks

[Documentation](README.md) | [Architecture](INTEGRATION.md#state-and-responsibility)

The supervisor will check each LLM before use
([#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585)).
No command runs the checks yet. Only a model with the status `passed` is clean. These checks are a defensive
layer. They do not prove that a model is safe. A clean result does not mean
that a modified model behaves like its original.

This page describes the checks that exist now. The chat template check, the
behavioral scan, the scan job, model selection in the desktop UI and the gate
of [#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583)
are not available yet. Thus no model can get `passed` yet. A model that passes the available checks stays `not_scanned`.

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
- No part of the path to the model directory can be a link. If the path has a
  link, or the supervisor cannot read the model directory, it stores no result.
  The model stays `not_scanned`.
- A file that the supervisor cannot read gives `error` with the reason
  `unreadable`.
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

The check reads each JSON file in one pass, as a stream. A JSON file above
128 MiB makes the model `unsupported`. The check rejects a file that is not
valid UTF-8 or not one strict JSON value, and a duplicate key. It also rejects
these keys at any depth outside token data:

- `auto_map`, `auto_mapping` and `custom_pipelines` (`remote-code-mapping`).
  These keys make a loader import code.
- `speculators_config`, `configuration_files`, `attribute_map`, `__dict__`,
  `audio_model_id` and `text_model_id` (`model-redirect`). With these keys, a
  loader loads another model or changes how it reads the configuration. For
  example, vLLM loads the models that `speculators_config`, `audio_model_id`
  and `text_model_id` name.
- `fast_tokenizer_files` and `transformers_weights` (`file-path-redirect`).
  These keys give other file names for tokenizer files or weights.

Token data has keys that are tokens, so a token such as `auto_map` is not a
configuration key:

- `vocab.json` and `added_tokens.json` hold token maps. The check applies no
  key or value rule to them.
- Below the top level, the values of the keys `vocab`, `merges`,
  `special_tokens` and `added_tokens_decoder` hold token data. The check
  applies no key or value rule to them.

At the top level of a JSON file, each file path key must be `null` or the
name of a loaded file in the model directory. The name cannot have `/` or a
backslash. Else the reason is `file-path-redirect`. These keys include `tokenizer_file`, `vocab_file`,
`merges_file`, `source_spm` and the other file names of the tokenizer classes
in transformers.

The key lists are in `internal/llmtraining/loader_keys.go`. The training
candidate inspection uses them too. The lists are not complete, so the check
also applies one rule to all other string values at any depth. A value fails
when it has one of these forms:

- It contains `..`, or it starts with `/`, `~`, `./` or a backslash
  (`file-path-redirect`).
- It is `README`, `README.md`, `LICENSE`, `LICENSE.md` or `.gitattributes`
  (`file-path-redirect`). The supervisor binds these files but never parses or
  shows them.
- It is the name of a model, tokenizer or code file, such as `x.model`, that is
  not in the model directory (`file-path-redirect`).
- It is a file name in a subdirectory, such as `sub/x.json`
  (`file-path-redirect`). A model has no subdirectories.
- It contains `://`, or it has the shape of a hub repository ID, such as
  `org/name` (`model-redirect`). A loader can download such a model.

The rule skips these values:

- All values below the key `chat_template`, also in its list form. The chat
  template check covers them.
- The values of the keys in `textKeys` in `internal/modelcheck/paths.go`:
  `content`, `additional_special_tokens`, `extra_special_tokens`,
  `bos_token`, `eos_token`, `unk_token`, `pad_token`, `sep_token`,
  `cls_token` and `mask_token`, also in lists.
- The values of `_name_or_path` and `name_or_path`. These keys hold a hub ID
  or the folder that saved the model. The transformers library and vLLM keep
  these values as labels and load no files or models from them.

The rule for file path keys and the value rule skip `tokenizer.json`, because
the tokenizers library reads no file names from it. Look at the release notes
of each new vLLM and transformers version for new keys of this kind.

A model without `.safetensors` weights or without `config.json` is
`unsupported`.

### Upstream identity

The supervisor reads the file tree of the source at the pinned commit from the
Hugging Face API. Each local file must be in that tree:

- For an LFS file, such as weights, the SHA-256 in the LFS pointer must be
  equal to the local SHA-256.
- For another file, the local size must be equal to the upstream size. Then
  the git blob ID (SHA-1) of the local bytes must be equal to the upstream git
  blob ID. The source publishes no other digest for these files. A file that
  is not in LFS can be at most 64 MiB. A larger file is a mismatch.

Attackers can make SHA-1 collisions. Thus the check accepts a git blob ID only
for these files:

- JSON files. The allowlist parses them as strict UTF-8 JSON.
- `README`, `README.md`, `LICENSE`, `LICENSE.md` and `.gitattributes`. No
  loader reads them.
- `.txt` and `.jinja` files with printable UTF-8 text only: no control
  characters other than tab, line feed and carriage return. Else the result
  is `upstream-identity-mismatch`.

Any other file, such as `tokenizer.model`, must be in LFS upstream with a
SHA-256. Else the check gives `findings` with the reason
`no-upstream-sha256`.

The upstream tree can have more files than the local directory. It can have
at most 32768 files.

The pinned commit must be on a branch or a tag of the repository. A commit
that only a pull request of the hub reaches does not pass, because any hub
user can make such a commit. The supervisor reads the branches and tags of the
repository. If no branch or tag points at the commit, it reads the commit
history of each branch, at most 64 pages in total. A refs list without
branches, or a branch or tag without a name or a commit, gives `error` with
the reason `upstream-invalid`. Else the check gives one of these results:

- The commit is in the history: the check continues.
- The commit is not in the history: `findings` with the reason
  `revision-not-on-branch`.
- The history has more pages: `error` with the reason
  `upstream-history-too-long`.

The supervisor reads at most 64 pages of the tree, of at most 16 MiB each,
over HTTPS. It does not follow redirects. It follows only next-page links to
the same path on the same host. A listing that has no other deadline stops
after 10 minutes.

For a gated or private repository, the supervisor uses the token of an
existing Hugging Face login. The first location that you configure decides.
The supervisor does not try a later location:

1. The `HF_TOKEN` variable, when it has a value.
2. The file at `HF_TOKEN_PATH`, when the variable has a value. The path must
   be absolute.
3. The file `token` in `HF_HOME`, when the variable has a value. The path must
   be absolute.
4. The file `huggingface/token` in `XDG_CACHE_HOME` when that path is absolute,
   else in `~/.cache`.

In step 3 or 4, a missing file means that you did not log in. The request
then has no token. A token file must be a regular file, not a link, of at most
4096 bytes. If the supervisor cannot use a token that you configured, the check
gives `error` with the reason `hub-token-unusable`. The reason holds no token
or path. The supervisor then sends no request.

It sends the token only to the hub. It never stores, logs or shows the token.

A failed listing gives `error` with one of these reasons:

| Reason | Cause |
| --- | --- |
| `upstream-not-found` | The repository or the commit does not exist. Without a token, the hub can also answer like this for a private repository |
| `upstream-denied` | The hub refused access |
| `upstream-moved` | The hub sent a redirect, for example for a renamed repository. Use its current name in the source |
| `upstream-unreachable` | A network failure, a server error, a timeout or rate limiting. Try again later |
| `upstream-invalid` | The listing breaks the rules above |
| `hub-token-unusable` | The supervisor cannot use the hub login token that you configured. See above |

## Results

| Status | Cause |
| --- | --- |
| `passed` | All checks passed. Only this status is clean |
| `findings` | A check found a problem in the files, for example a rejected file, a remote code key, invalid JSON or an identity mismatch |
| `error` | A check did not complete. Causes are a file that the supervisor cannot read (`unreadable`), a failed upstream listing or a file that changed during the check (`files-changed`) |
| `unsupported` | The checks do not support the model, for example a model without `.safetensors` weights or with a JSON file above the limit |
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

The supervisor calculates the status again from a new inventory of the files
on disk. An inventory problem that the result does not hold, such as a new
link, gives `not_scanned`. If a file
digest, the directory, the source, the checker version or the configuration
(for example the hub) differs from the result, the status is `not_scanned`. A
result of another record version is also `not_scanned`. Thus a new checker
version checks each model again.

A canceled check stores no result. The model stays `not_scanned`.

No signatures or keys protect the results. The deployment trusts processes
with the service UID (see [SECURITY.md](../SECURITY.md)). A process with that
UID can forge a result. This is an accepted limit.
