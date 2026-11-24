# Model checks

[Documentation](README.md) | [Architecture](INTEGRATION.md#state-and-responsibility)

The supervisor will check each LLM before use
([#585](https://github.com/mickey-kras/llm-workload-supervisor/issues/585)).
Only a model with the status `passed` is clean. These checks are a defensive
layer. They do not prove that a model is safe. A clean result does not mean
that a modified model behaves like its original.

This page describes the checks that exist now. The behavioral scan and the
scan job of #585 are not available yet. No model can get `passed` yet. A
model that passes the available checks stays `not_scanned`. The
[launch gate](#launch-gate) refuses a model whose stored result does not
allow the launch.

Setup shows the stored status. See [Status display](#status-display). The
operator runs the checks of a workload from setup. See
[Running checks](#running-checks).

The checks read only the local model files. They make no network requests.

## Model

A model is one local directory. Another directory is another model. A file
that changes makes another model too. Results never carry over.

The model ID is 1 to 128 ASCII characters: letters, digits, `.`, `_` and `-`.
The first character is a letter or a digit.

## Inventory

The supervisor reads the top level of the model directory and calculates the
SHA-256 of each file. It applies the digest and shard index rules in
`internal/modelcheck/safetensors.go`:

- A model has at most 4096 files. A model with more files is `unsupported`.
- No part of the path to the model directory can be a link. If the path has a
  link, or the supervisor cannot read the model directory, it stores no result.
  The model stays `not_scanned`.
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
  `audio_model_id`, `text_model_id`, `audio_tokenizer`,
  `audio_tokenizer_class` and `audio_tokenizer_name_or_path`
  (`model-redirect`). With these keys, a loader loads another model or changes
  how it reads the configuration. For example, vLLM loads the models that
  `speculators_config`, `audio_model_id` and `text_model_id` name. The
  transformers processor loader loads the audio tokenizer class and path that
  the `audio_tokenizer` keys name.
- `fast_tokenizer_files` and `transformers_weights` (`file-path-redirect`).
  These keys give other file names for tokenizer files or weights.
- `chat_template` (`misplaced-chat-template`), other than at the top level of
  `chat_template.json`, `tokenizer_config.json` and `processor_config.json`.
  The chat template check reads only these keys.

Token data has keys that are tokens, so a token such as `auto_map` is not a
configuration key:

- `vocab.json` and `added_tokens.json` hold token maps. The check applies no
  key or value rule to them.
- Below the top level, the values of the keys `vocab`, `merges`,
  `special_tokens` and `added_tokens_decoder` hold token data. The check
  applies no key or value rule to them.

At the top level of a JSON file, each file path key must be `null` or the
name of a loaded file in the model directory. The name cannot have `/` or a
backslash. Other values give `file-path-redirect`.
These keys include `tokenizer_file`, `vocab_file`, `merges_file`,
`source_spm` and the other file names of the tokenizer classes in
transformers.

See `internal/modelcheck/safetensors.go` for the key lists. The check
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

- All values below the top-level key `chat_template` of the template files,
  also in its list form. The chat template check covers them.
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

### Chat template

The check starts only after the file allowlist passed. It reads each file
again and compares its SHA-256 with the inventory.

These files can hold the template:

- `chat_template.jinja`: the template text.
- `chat_template.json`: the top-level key `chat_template`. The file must
  have this key.
- `tokenizer_config.json` and `processor_config.json`: the top-level key
  `chat_template`, when it has a value.

Each file that holds a template must hold the same text. The value must be one
string, because the supervisor pins one template. The [rules](#rules) give
the reasons.

The check does not read the structure of the template. vLLM runs the
template in the sandboxed Jinja environment of transformers. The sandbox
blocks access to Python internals, files and the network.

#### Rules

The result gives the reason of the first problem that the check finds:

| Reason | Rule |
| --- | --- |
| `no-chat-template` | No file holds a template (`unsupported`) |
| `named-chat-templates` | The template is a list of named templates (`unsupported`) |
| `chat-template-too-large` | The template is above 256 KiB (`unsupported`) |
| `chat-template-invalid` | The template is empty or not a string, or `chat_template.json` has no template. Or a JSON file that can hold the template is not a JSON object. These files are `chat_template.json`, `tokenizer_config.json` and `processor_config.json` |
| `chat-template-conflict` | Two files hold different templates |

#### Result

When the check passes, the result holds the SHA-256 of the template text,
as UTF-8 bytes. For `chat_template.jinja`, this is the SHA-256 of the file.
The result also names the first file that holds the template.

The [hardened vLLM launch](INTEGRATION.md#hardened-vllm-launch) records the
SHA-256 of the file that `--chat-template` names. For a file with the same
text, the two digests are equal. The [launch gate](#launch-gate) compares
them.

The managed launch reads `chat_template.jinja` from the model folder. A model
that has its template only in a JSON file has no such file. The managed
launch does not support such a model. An existing launch can name a file
outside the model folder with `--chat-template`. Write the template text of
the JSON file into that file without changes. Then the two digests are
equal.

The check does not find an instruction in the template text. The behavioral
scan covers the model, not the template.

## Results

| Status | Cause |
| --- | --- |
| `passed` | All checks passed. Only this status is clean |
| `findings` | A check found a problem in the files. Examples are a rejected file, a remote code key, invalid JSON and two different chat templates. A chat template rule that fails is also a finding, other than the reasons that the table marks `unsupported` |
| `error` | A check did not complete. Causes are a file that the supervisor cannot read (`unreadable`) or a file that changed during the check (`files-changed`) |
| `unsupported` | The checks do not support the model, for example a model without `.safetensors` weights, with a JSON file above the limit or without a chat template |
| `not_scanned` | A check did not run, the files changed after the check, the checker changed, or there is no result |

The supervisor keeps one result for each model in its private state database,
as bounded strict JSON of at most 4 MiB. A result holds these items:

- The model ID and the directory.
- The SHA-256 and the file identity of each file.
- The status, the coverage, and the outcome and reason code of each check.
  Coverage `complete` means that all checks ran. Coverage `partial` means
  that one or more checks did not run.
- The SHA-256 of the chat template text and the name of its file, when the
  chat template check passed. The result holds no template text.
- The checker version and the configuration: the file limits and the
  template size limit.
- The check time.

Reason codes never hold paths or file content. The result format has
version 3. A result with another positive version is `not_scanned`. A
result without a positive version is not valid. Thus a result of version 2
reads as `not_scanned` until you check the model again.

The supervisor calculates the status again from a new inventory of the files
on disk. The status is `not_scanned` when one of these differs from the
result:

- The inventory problems, for example a new link.
- A file digest.
- The directory.
- The checker version. Thus a new checker version checks each model again.
- The configuration.

A check that does not finish stores no result, and the last stored result
stays. See [Running checks](#running-checks).

No signatures or keys protect the results. The deployment trusts processes
with the service UID (see [SECURITY.md](../SECURITY.md)). A process with that
UID can forge a result. This is an accepted limit.

## Status display

The desktop setup shows the status of each workload. The status is on the
main page of the application settings and in the review summary. The
`gpu-setup model-status` action reads it from the stored results. This
action changes nothing and runs no check.

| Display | Cause |
| --- | --- |
| Scanned, no findings | The status is `passed` |
| Not scanned | The status is `not_scanned`, there is no result, or setup cannot read the stored result |
| Findings detected | The status is `findings` |
| Scan failed | The status is `error` |
| Scan not available for this model | The status is `unsupported` |
| No backdoor scan available for image models | The workload is a ComfyUI workload |
| Model files not identified | Setup cannot identify the model files of the workload |
| Scan status not available | Setup cannot run `gpu-setup model-status`, the action fails, or the report has a version that setup cannot read |
| Reading scan status… | Setup did not get the status yet |

Each state has an icon and a text, so the state does not depend on color.
The display never calls a model safe. When the file and template checks
passed, a second line shows "File checks passed. Behavioral scan not
available yet." A second line also shows the cause of a `not_scanned`
status, for example files that changed or another checker version.

The status applies to the model of the accepted workload. Each entry holds
a SHA-256 of the model directory path or of the model ID, and the chat
template pin of the accepted launch. This SHA-256 is not a digest of the
files. If a selection or an interrupted setup has another model, setup
shows "Not scanned".

After setup applies the new model, the stored result of the workload
applies again, with these rules:

- A managed launch with another model directory reads as `not_scanned`
  (`other-directory`) until a check stores a new result.
- A launch that names only a model ID has no model directory. The stored
  result of its workload ID stays in use until the next check.

A result whose chat template check passed applies only to a launch that
pins the same template SHA-256. If the SHA-256 is different, setup shows
`not_scanned` with "The chat template of the launch is not the checked
template." A launch without a pin pins no template. Other results with findings or an
error keep their status.

The review summary compares the pin of the launch. The settings card adds
only a comparison of the model, because a selection has no pin before its
preview. The pin of the accepted launch still applies there.

For a result that does not apply, the details show its checks as the last
stored result.

"Scan details" shows the checker revision, the scanner revision, the
coverage, the check date and the outcome of each check. The scanner
revision is "not available" until the behavioral scan is available. The
details also show this note: "These checks are a defensive layer, not
complete backdoor detection."

The status uses the stored digests. It does not hash the model files each
time that setup opens, because the weights can be large.

The button `Check files again` calculates the digests of one workload again
and compares them with the result. Files that changed, or that setup cannot
read, for example a missing folder, give `not_scanned`. The operator can
cancel this check. A canceled or failed
check keeps the last status. Setup does not store the result of this
check.

Setup writes the error of a failed action to its log.

The review summary also shows a note when the stored result of a workload
blocks its launch. The note has a link to the Model scan card of the
workload. A result blocks the launch when the [launch gate](#launch-gate)
refuses it.

The note tells the operator what to do:

- For a result with findings, an error or an unsupported model, it asks you
  to read the cause on the card.
- For a selection that changes the model folder or the chat template, it
  asks you to run the checks again after you finish setup.
- For an accepted launch that pins another chat template than the checked
  template, it asks you to run setup again for the workload. You must also
  run the checks again.
- For other causes, it asks you to run the checks again.

The card and "Scan details" show the cause of a result in plain words, for
each reason code. For an unknown reason code, they show the code.

## Running checks

The Model scan card of each accepted LLM workload has the button
`Run checks`. It runs `gpu-setup model-check` for that workload. The
action runs the file allowlist and the chat template check on the model
directory of the accepted launch. It stores the result with the workload ID
as the model ID. Setup then shows the new status.

Before you start a run, read the warning on the card. Some results block
the launch of the workload until a new check allows it. These are an error,
findings and an unsupported model.

A cache folder of Hugging Face holds links to its files, and links are a
finding of the [file allowlist](#file-allowlist). For a model in the cache,
download a copy with `hf download --local-dir` and select that folder.

A run has a time limit of 60 minutes. These rules apply to the result of a
run:

| Cause | Stored result |
| --- | --- |
| The run finishes | The result replaces the stored result. This includes findings and errors |
| The operator cancels the run, or an interrupt or a stop signal cancels it | Nothing. The last stored result stays |
| The run reaches its time limit | Nothing. The error says that the run stopped at the time limit |
| A signal kills the process of the run | Nothing. Setup writes a result in one database transaction |
| The state lock stays busy until the time limit | Nothing. The card shows the cause "Setup was busy with another change" |
| The model directory or the chat template pin changed during the run | Nothing. The action fails: run the checks again |
| The run stops after setup stored its result | The card reads the stored status again and says so |

You can run the checks while the workload runs. The checks only read the
model files.

Setup writes the result as the other setup steps write the state. It needs
an activated setup, and it refuses a write during maintenance or for
another installed release. It holds the state lock only for the write.
Before the write, it reads the accepted workload again.

Another setup change or a workload switch can hold the state lock. Then a
finished run waits for the lock until its time limit.

When an action fails, the card names the cause, for example a busy setup or
maintenance. When setup had no stored result before the run, the card says
"Setup stored nothing."

Setup refuses a run for these workloads:

- An LLM workload that names only a model ID, such as an adopted service.
  Setup cannot identify its model files. The card shows no `Run checks`
  button for it.
- An image workload or a workload that setup cannot identify.
- A workload that is not in the accepted configuration.

To run the checks yourself, run `gpu-setup model-check` as the desktop
account. Give the request on standard input, for example
`{"version":1,"workload":"chat"}`. The action writes one status entry
object, in the form of an entry of the `gpu-setup model-status` report.

The request is strict JSON of at most 4 KiB.

## Launch gate

The supervisor checks the stored result before it starts a vLLM workload.
Switch, ownership changes and activation run this gate before they change
the state. A refused launch changes nothing, and the error names the
workload, its unit and the cause. The stored result of a workload uses the
workload ID as its model ID.

A stored result allows the launch only when all of these are true:

- The result is valid.
- The file allowlist and the chat template checks passed. The behavioral
  scan is not available yet, so it can stay `not_run`.
- For a managed launch, the result is for the model directory of the
  launch.
- The binding pins a chat template, and its SHA-256 is the
  [template SHA-256](#result) of the result. A template that changed after
  the check refuses the launch.
- The model directory holds exactly the checked files. Each file has the
  file identity of the check: size, device, inode, modification time and
  change time.

A result that the supervisor cannot read refuses the launch, and so does a
vLLM workload without a stored result. Run the checks in setup before the
first launch. An adopted vLLM service names no model directory, so setup
cannot check it and the gate refuses it. Set it up as a supervisor-managed
launch before you upgrade. A result that a release before this gate stored
has no file identities, so it also refuses the launch until you check the
model again.

The gate does not hash the model files again, because the weights can be
large. The check records each file identity with its digest, from the same
open file before and after the read. A write, a replacement, an added,
removed or linked file changes the identity or the file list. No user
process can set the change time back. Thus the gate refuses changed files
without reading them.

Image models (ComfyUI) have no gate. ComfyUI selects models while it runs,
and custom nodes run code, so a launch check of its model files gives no
real protection. To isolate ComfyUI, sandbox its unit.

The gate applies only to new launches. A workload that runs during an
upgrade keeps running. A recovery that only checks the state starts
nothing, so it skips the gate. A rollback start of the previous workload
after a failed catalog change also skips it.
