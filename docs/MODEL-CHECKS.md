# Model checks

[Documentation](README.md) | [Architecture](INTEGRATION.md#state-and-responsibility)

The supervisor will check each LLM before use
([#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585)).
No command runs the checks yet. Only a model with the status `passed` is clean. These checks are a defensive
layer. They do not prove that a model is safe. A clean result does not mean
that a modified model behaves like its original.

This page describes the checks that exist now. The behavioral scan, the scan
job, model selection in the desktop UI and the gate of
[#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583)
are not available yet. No model can get `passed` yet. A model that passes the available checks stays `not_scanned`.

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

1. If `HF_TOKEN` has a value, the `HF_TOKEN` variable.
2. If `HF_TOKEN_PATH` has a value, the file at `HF_TOKEN_PATH`. The path must
   be absolute.
3. If `HF_HOME` has a value, the file `token` in `HF_HOME`. The path must
   be absolute.
4. If `XDG_CACHE_HOME` is an absolute path, the file `huggingface/token` in
   `XDG_CACHE_HOME`. Else the file `huggingface/token` in `~/.cache`.

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

### Chat template

The supervisor has its own static checker for the chat template. It reads
the template as text and never runs it. The check starts only after the
upstream identity check passed. Thus the template has the same bytes as
the template of the source at the pinned commit. The check reads each file
again and compares its SHA-256 with the inventory. A file that changed gives
`error` with the reason `files-changed`.

These files can hold the template:

- `chat_template.jinja`: the template text.
- `chat_template.json`: the top-level key `chat_template`. The file must
  have this key.
- `tokenizer_config.json` and `processor_config.json`: the top-level key
  `chat_template`, when it has a value.

Each file that holds a template must hold the same text. The value must be one
string. A list of named templates makes the model `unsupported`, because the
supervisor pins one template. A model without a template is `unsupported`.

#### Text sources

vLLM runs the template in the sandboxed Jinja environment of transformers.
The checker accepts only the subset of Jinja in
[Supported subset](#supported-subset). Each other construct fails. With
this subset, the template can write only text from these sources:

- The literals of the template: its string literals and its text outside
  tags. An operator reads the literals that need review, see
  [Review](#review).
- The request: the messages, the tools, the documents and the chat template
  arguments, such as `enable_thinking`.
- The special tokens of the tokenizer.

#### Supported subset

| Construct | Supported | Reason when it fails |
| --- | --- | --- |
| Statements | `if`, `elif`, `else`, `for`, `set`, a `set` block, `macro`, `break`, `continue` and `generation` | `chat-template-statement` |
| Names | `messages`, `tools`, `documents`, `add_generation_prompt`, the special token names such as `bos_token` and `image_token`, `loop` with an attribute, the names that the template assigns, and other request names such as `enable_thinking` | `chat-template-unsafe-name` for the names in [Rules](#rules) |
| Functions | `raise_exception`, `strftime_now`, `namespace`, `range`, `dict` and the macros of the template. `range`, `namespace` and `dict` only as in the rules below | `chat-template-unknown-source` |
| Methods | `items`, `keys`, `values`, `get`, `strip`, `lstrip`, `rstrip`, `split`, `rsplit`, `splitlines`, `startswith`, `endswith`, `lower`, `upper`, `title`, `capitalize`, `replace`, `join` and `count` | `chat-template-unknown-source` |
| Attributes | An attribute without a call, other than the name of a Python method, for example `m.content`. An attribute of `loop`, such as `loop.index`, other than `cycle` and `changed` | `chat-template-unknown-source` |
| Constants | `true`, `false` and `none`, also `True`, `False` and `None`. They can be in an `if` or `elif` statement, in an inline `if` condition or in the `if` condition of a `for` statement. They can be a direct operand of a comparison or the argument of a test. They can also be the whole value of a filter keyword that only switches a behavior, such as `boolean`, `reverse` or `first`. In other places, the rule for values that are not text below this table applies | `chat-template-unknown-source` |
| Filters | `abs`, `batch`, `capitalize`, `center`, `count`, `d`, `default`, `dictsort`, `e`, `escape`, `first`, `float`, `forceescape`, `groupby`, `indent`, `int`, `items`, `join`, `last`, `length`, `list`, `lower`, `map`, `max`, `min`, `reject`, `rejectattr`, `replace`, `reverse`, `round`, `safe`, `select`, `selectattr`, `slice`, `sort`, `string`, `sum`, `title`, `tojson`, `trim`, `truncate`, `unique`, `upper`, `urlencode`, `wordcount` and `wordwrap` | `chat-template-unknown-source` |
| Tests | `boolean`, `callable`, `defined`, `divisibleby`, `eq`, `equalto`, `escaped`, `even`, `false`, `filter`, `float`, `ge`, `gt`, `in`, `integer`, `iterable`, `le`, `lower`, `lt`, `mapping`, `ne`, `none`, `number`, `odd`, `sameas`, `sequence`, `string`, `test`, `true`, `undefined` and `upper` | `chat-template-unknown-source` |
| `map` | `map(attribute=..., default=...)`, or `map` with one of these filter names and no other argument: `trim`, `lower`, `upper`, `title`, `capitalize`, `string`, `int`, `float`, `length`, `tojson`, `e` and `escape` | `chat-template-unknown-source` |
| `select`, `reject`, `selectattr`, `rejectattr` | The test name is one string literal | `chat-template-unknown-source` |
| `%` | Only an integer modulo, for example `loop.index0 % 2`. The whole left operand is an integer or an integer `loop` attribute, with an optional sign. The integer attributes are `index`, `index0`, `revindex`, `revindex0`, `length`, `depth` and `depth0`. The right operand is an integer | `chat-template-unknown-source` |
| Part selection | Subscripts, slices and `x.0`, and the filters `first`, `last`, `min`, `max`, `truncate`, `slice` and `batch`. Also `select` and `reject` with a test name, an attribute path with an integer part, the iterable of `for`, and the methods and filters that remove characters. Only on request data, see [Part selection](#part-selection) | `chat-template-unknown-source` |
| Other operators | Comparisons, `in`, `is`, `and`, `or`, `not`, `~`, `+`, `-`, `*`, `/`, `//`, `**` and inline `if` | |

These rules apply too:

- A template must not give a function name or `loop` another value, for
  example with a macro, a macro argument, `set` or `for`
  (`chat-template-unsafe-name`). It can use a function name only in a call,
  and `loop` only with an attribute, for example `loop.index0`. It can also
  use these names before a test such as `is defined`, or as a keyword
  argument of a call. The `%` rule trusts that `loop` is the loop.
- A template can use its own macros only in a call. Another use of a
  macro fails with `chat-template-unknown-source`. Examples are
  `x.arguments`, `set y = x`, and a macro argument or a keyword argument
  with the name of a macro. The attributes of a macro give its names as
  text. A macro argument with the name of a macro gives that name another
  value, and a call of the name then calls that value.
- A macro must not use `kwargs`, `varargs` or `caller`
  (`chat-template-unknown-source`). `kwargs` gives the keyword names of the
  call as text.
- A namespace must not get an attribute with the name of a method in the
  Methods row of the table, for example `namespace(get=x)` or `set ns.get = x`
  (`chat-template-unsafe-name`). The attribute replaces the method.
- A call of a value that is not a name, for example `x['decode']()`, fails
  with `chat-template-unknown-source`.
- An unpacked argument, for example `f(*x)` or `f(**x)`, fails with
  `chat-template-unknown-source`.
- `map` must not name `map`, `select`, `reject`, `selectattr` or
  `rejectattr`.
- An attribute with the name of a method of a Python value must have a
  call, for example `x.items()` and not `x.items`
  (`chat-template-unknown-source`). These values are str, list, tuple,
  dict, int, float, Markup, a generator and the loop object. The text of a
  method names it. To read a key with such a name, use `get`, for example
  `x.get('title')`.
- A string key must not be the name of such a method, for example
  `x['title']`. If the value has no item with a string key, Jinja gives the
  attribute with that name. The same applies to each part of a filter
  attribute, for example `map(attribute='content.title')`
  (`chat-template-unknown-source`).
- A key or a filter attribute that is not one string literal can have only
  request names, integer `loop` attributes, numbers and operators
  (`chat-template-unknown-source`). Thus a literal or a macro cannot give
  the name of a method. This rule does not apply to a slice.
- `range(...)` must be in an `if` or `elif` statement, be the iterable of
  `for`, be the right operand of `in`, or go to a filter that consumes it
  (`chat-template-unknown-source`).
- `namespace(...)` and `dict(...)` must be the whole value of `set`
  (`chat-template-unknown-source`). A template can use a name that `set`
  gives a namespace only with an attribute, for example `ns.found`, or
  before a test.
- The filters `batch`, `map`, `select`, `reject`, `selectattr`,
  `rejectattr`, `slice`, `items`, `unique` and `reverse`, and the methods
  `items`, `keys` and `values`, give an iterator or a view. Each must be in
  an `if` or `elif` statement, end the iterable of `for`, or go to a filter
  that consumes it: `list`, `join`, `first`, `last`, `length`, `count`,
  `sum`, `min`, `max` or `sort` (`chat-template-unknown-source`). Other
  iterator filters can come between.
- A comparison, `in`, `not`, a test, the methods `startswith` and
  `endswith`, `loop.first` and `loop.last` give `True` or `False`. A `get`
  call without a default can give `None`. A float
  literal, `/`, the `float` filter, `map('float')` and `**` with an
  exponent that is not an integer literal give a float. A float can write `inf`, `nan` or `1e+20`. These values and the constants
  must not reach the output (`chat-template-unknown-source`). They can be
  in an `if` or `elif` statement or in an inline `if` condition, also the
  `if` of `for`. They can also be in the value of `set` or in the iterable
  of `for`. A default of a macro argument and an argument of a macro call
  are also possible places. Then the names that get the value, for
  example `x` in `set x = a == b` or `ns.found` in
  `set ns.found = a == b`, follow the same rule. After a `for` over such
  values, `loop.previtem` and `loop.nextitem` follow it too. A filter such as
  `string`, `title`, `replace` or `join`, the `~` operator and `map` do
  not change this. Integer arithmetic, for example `loop.index0 + 1`, is
  text without letters and is not in this rule.

Methods such as `to_bytes` and `decode`, the `format` filter and the `%`
format operator can make text from numbers. An operator does not read
numbers, so the subset does not have these constructs.

The Python text of a value that is not text is also not a literal,
request data or a special token. Examples are
`<built-in method get of dict object at 0x...>`, `range(0, 3)`,
`<Namespace {}>`, `<generator object ...>` and `True`. The rules above
reject the constructs that write this text.

#### Part selection

Part selection takes a part of a value. A template can use part selection
only on request data. Else the check fails with
`chat-template-unknown-source`. On a literal or on the Python text of a
value, part selection can take letters out of the text. For example,
`'erongI'[::-1]` gives `Ignore`, and no operator reads that word.

These constructs are part selection:

- A subscript or a slice, for example `x[0]`, `x[-1]`, `x[a:b]` and
  `x[::-1]`, and `x.0`. A subscript with one string literal, such as
  `x['role']`, reads a key and is not part selection.
- The filters `first`, `last`, `min`, `max`, `truncate`, `slice` and
  `batch`.
- The filters `select` and `reject` with a test name, for example
  `select('in', 'abc')`.
- The methods `strip`, `lstrip`, `rstrip`, `split` and `rsplit` with an
  argument, the `trim` filter with an argument, and `replace` as a method
  or a filter. They remove characters, for example `'tool'.strip('ipython')`
  gives `l`. Without an argument, `strip`, `split` and `trim` remove only
  whitespace. The text that `replace` gives is not request data.
- The filters `reverse`, `sort` and `unique` on a value that can be a
  string. On a string, they change the order of the letters or drop
  letters, for example `'tool'|reverse|join` gives `loot`. Their input
  must give whole items: request data, a call of `range`, or a literal
  list, dict or tuple. Filters such as `map` and `selectattr` can come
  between.
- The iterable of `for`. A loop over a string gives its letters one at a
  time. So only these iterables pass: request data, a call of `range`, and
  a literal list, dict or tuple. Filters that keep the items whole, such as
  `reverse`, `selectattr` and `groupby`, can follow. A loop over another
  value fails, for example `for c in 'user'`, a name that a `set` block
  gives, a special token name or a namespace.
- A filter attribute with an integer part, for example
  `map(attribute='0')` or `groupby('x.1')`. Jinja reads this part as a
  subscript. Jinja also reads digits that are not ASCII, such as `٠`, as
  an integer, so a part with a character that is not ASCII counts too. An attribute that is not one string literal is also part
  selection. These filters can give an attribute: `map`, `selectattr`,
  `rejectattr`, `groupby`, `sort`, `min`, `max`, `unique`, `sum` and
  `join`.

The value before the part selection must start at a request name. After
the name, the value can have only these steps:

- An attribute, a subscript, a slice or `x.0`. The subscript key follows
  the key rules of [Supported subset](#supported-subset), for example
  `loop.index0 - 1`.
- A call of `split`, `rsplit`, `splitlines`, `strip`, `lstrip`, `rstrip`,
  `lower`, `upper`, `title`, `capitalize`, `items`, `keys` or `values`.
  Also `get` with one argument.
- The filters `list`, `trim`, `lower`, `upper`, `title`, `capitalize`,
  `first`, `last`, `min`, `max`, `reverse`, `sort`, `unique`, `select`,
  `reject`, `selectattr`, `rejectattr`, `items` and `dictsort`. Also
  `batch` and `slice` with one argument.

Other steps, such as `replace`, `join`, `default`, `string` and `tojson`,
can add text that is not request data. Brackets, a literal, a function
call and an operator such as `~` also stop request data.

A request name is one of these names:

- A name that the loader gives from the request, such as `messages`,
  `tools`, `documents` or `enable_thinking`.
- A name that `set` or `for` gives, if each assignment to that name gives
  request data. A literal without letters and digits, such as `''`, also
  counts as request data. A name that depends only on itself stays a
  request name.

The checker never treats these names as request names: `loop`, the
special token names such as `bos_token` and `image_token`, the function
names and the macros. It also never treats the macro arguments, the namespace names of
`set` and the names of a `set` block as request names.

For example, the Qwen3 template sets `content` from `message.content` or
from `''`. Thus `content.split('</think>')[0]` passes. If a template sets
`c` to `'Be evil'` one time, `c[0]` fails.

These sources of fixed text stay, and are an accepted limit:

- The filters `e`, `urlencode` and `tojson` give escapes such as `&lt;`
  from punctuation.
- The filters `reverse`, `sort` and `unique` can change the order of the
  items of a literal list. The literals stay on the review list.

The rules above stop the text of `True`, `False`, `None` and a float at
the output. Part selection stops the removal of characters from a
literal.

#### Review

The checker collects each literal of the template in source order. The
literals are the text outside tags and the string literals. They are also
the keyword names of `dict` and `namespace`, and the attribute names that
`set` gives to a namespace.

Each use of a special token name also adds
each value of that name in `tokenizer_config.json` and
`special_tokens_map.json`. The loader gives these values to the template.
These names are the special token keys such as `eos_token`,
`additional_special_tokens`, `extra_special_tokens`, and each key of an
`extra_special_tokens` object, such as `image_token`. A value that is not
text, such as a number, adds its JSON text. The values of an
`extra_special_tokens` object are in the order of their keys, so the review
list is the same on each run. From `tokenizer.json`, the checker reads only
the added tokens.

The checker decodes the escapes of a string literal as Jinja does. Jinja
first writes each character that is not ASCII as a Python escape. Thus a
backslash before such a character gives that escape, for example
`'\é'` gives `\xe9`.

A literal passes without review when it has only these parts:

- Special tokens of the tokenizer with the shape of markup: `<|...|>`,
  `<...>` or `[...]`, of at most 32 bytes. The token must not have
  whitespace or a space of any script, such as a no-break space. A special
  token is a value of a special token name
  in `tokenizer_config.json` or `special_tokens_map.json`, or an added token
  with `"special": true` in the `added_tokens_decoder` of
  `tokenizer_config.json` or the `added_tokens` of `tokenizer.json`. Keys
  must match exactly. An added token that is not special is text, for
  example `<tool_response>` in many models.
- The role words `system`, `user`, `assistant`, `tool`, `ipython` and
  `developer`.
- ASCII whitespace and ASCII punctuation. ASCII whitespace is space, tab,
  line feed and carriage return. ASCII punctuation is
  ``!"#$%&'()*+,-./:;<=>?@[\]^_`{|}~``. Each other character needs review.
  Examples are other spaces, symbols such as `Ⓘ` and `→`, marks, and the
  letters and digits of all scripts.

For example, if `<|im_start|>` is a special token, `'<|im_start|>assistant\n'`
passes.

A literal also passes without review when it has only the Qwen3 tags
`<think>`, `</think>`, `<tool_call>` and `</tool_call>`, ASCII whitespace
and ASCII punctuation. The tokenizers of Qwen3 do not mark these tags as
special. The tags must match exactly. Thus `<thinkx>`, `<THINK>` and
`<tool_call >` need review. This rule does not join with special tokens
or role words, so `<think>user` needs review. No other tag passes by this
rule.

Each other literal needs review, except in these positions, where it cannot
reach the output:

- A subscript key, `x['k']`.
- The first argument of `get`, `x.get('k')`. The default value needs
  review.
- A direct operand of `==`, `!=`, `in` or `not in`, also an item of a literal
  list that is such an operand, and the argument of a test. An operand with
  `~`, `+`, a filter or a test is not direct, because these bind more
  strongly than a comparison.
  The `in` of a `for` statement is not a comparison. The loop writes the
  items of its iterable, so a literal in the iterable needs review.
- The attribute and test arguments of `selectattr` and `rejectattr`, and the
  attribute of `map` and `groupby`. A `default` value needs review.
- The message of `raise_exception`. The message goes to the error, not to
  the prompt.
- A format of `strftime_now` with only `%` directives (`%` and one ASCII
  letter, or `%%`), ASCII whitespace and ASCII punctuation.

All other positions need review. Examples are the arguments of `replace`,
`split` and the other methods, and the arguments of `join`, `default`, `sum`
and the other filters. A value that `set` gives to a name also needs review.
A filter or a test that `map`, `select`, `reject`, `selectattr` or
`rejectattr` calls by name must be in the lists above. It must not be in the
list of the `unsafe-name` rule.

A trigger word in a condition, such as `'xyzzy' in content`, does not need
review. This is an accepted limit. Each literal that the condition controls
and that can reach the output needs review.

Each literal on the review list has its line number, in source order. Thus
fragments of one text, such as `'ign' ~ 'ore'`, are next to each other. The
list holds at most 4096 literals and at most 1 MiB as JSON. A larger list
makes the model `unsupported`.

The line number counts the lines of the template text after JSON decoding.
A JSON file holds the template on one line, so the line number is not a
line of that file. The result names the first file that holds the
template, for example `tokenizer_config.json`.

A template with an empty list passes. A template with a list passes only
when an operator read the list and approved it. The approval holds the
SHA-256 of the template text, the SHA-256 of the review list, the time and
the UID of the operator. No approval is automatic.

A stored approval applies again only to the same model ID, directory,
source, template text and review list. A changed template, another source or
directory, or a new checker version that gives another review list asks
again. A new checker version that gives the same review list keeps the
approval.

The approval is a part of the stored result. Each run of the checks
replaces the stored result. A run that stops before the template check,
for example after an upstream error, stores no approval. A result that
you delete before a run starts also drops the approval. A run that is in
progress during the delete can store the approval again, because that run
read it before the delete. The next run that reaches the template check
asks again.

Without an approval, the template check gives `findings` with the reason
`chat-template-needs-review`. The result holds the review list, so that an
operator can approve it. A refused approval gives the same result. An
approval that fails for another cause gives `error` with the reason
`chat-template-approval-failed`. An error when the supervisor reads a
stored approval is such a cause. The model selection of
[#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585)
will show the list and ask for the approval.

#### Rules

The result gives the reason of the first problem that the checker finds:

| Reason | Rule |
| --- | --- |
| `no-chat-template` | No file holds a template (`unsupported`) |
| `named-chat-templates` | The template is a list of named templates (`unsupported`) |
| `chat-template-too-large` | The template is above 256 KiB (`unsupported`) |
| `chat-template-too-complex` | The template has more than 64 nested blocks, or a tag has more than 2048 tokens. Or the template uses a special token name with a value above 256 bytes (`unsupported`) |
| `chat-template-too-many-literals` | The review list has more than 4096 literals (`unsupported`) |
| `chat-template-review-too-large` | The review list is above 1 MiB as JSON (`unsupported`) |
| `chat-template-invalid` | The template is empty or not a string, or `chat_template.json` has no template. Or a JSON file that can hold the template is not a JSON object. These files are `chat_template.json`, `tokenizer_config.json` and `processor_config.json` |
| `chat-template-conflict` | Two files hold different templates |
| `chat-template-hidden-text` | The template has text that a reader does not see. This includes invalid UTF-8 and control characters other than tab, line feed and a carriage return before a line feed. A bare carriage return fails, because on a terminal the text after it writes over the text before it. It also includes the replacement character U+FFFD and format characters, such as zero-width and bidirectional controls, and tag characters. Private use characters, line and paragraph separators and variation selectors also fail. The checker decodes the escapes of string literals and applies the rule again. The rule also applies to the value of a special token name that the template uses. A named escape (`\N{...}`) fails |
| `chat-template-syntax` | A tag, a string or a comment does not close, or brackets do not match. A block does not close, or it closes in the wrong order |
| `chat-template-statement` | The template uses a statement that is not in this list: `if`, `elif`, `else`, `for`, `set`, `macro`, `generation`, `break` and `continue`. Their end statements are also in the list. For example, `include`, `import`, `extends`, `raw`, `call` and `filter` fail |
| `chat-template-unsafe-name` | The template uses an attribute that starts with `_`, a name or a string with `__`, or one of `self`, `cycler`, `joiner`, `lipsum`, `attr`, `format`, `format_map`, `mro` and `random`. These names give access to Python objects, write their own text or make the output change from one run to the next. The template also gives a function name or `loop` another value. Or it gives a namespace an attribute with the name of a method in the Methods row, see [Supported subset](#supported-subset) |
| `chat-template-unknown-source` | The template uses a construct outside the [Supported subset](#supported-subset) that can bring in text that no operator reads. This includes part selection on a value that is not request data |
| `invalid-json` | A tokenizer file that defines special tokens is not a JSON object |
| `chat-template-needs-review` | No operator approved the review list |
| `chat-template-approval-failed` | The approval failed for a cause other than a refusal (`error`) |
| `chat-template-check-failed` | The checker failed on the template. This is a fault of the checker, not of the template (`error`) |

#### Result

When the structural rules pass, the result holds the SHA-256 of the
template text, as UTF-8 bytes. For `chat_template.jinja`, this is the
SHA-256 of the file. An approval that fails
(`chat-template-approval-failed`) leaves no SHA-256 and no review list in
the result.

The [hardened vLLM launch](INTEGRATION.md#hardened-vllm-launch) records the
SHA-256 of the file that `--chat-template` names. For a file with the same
text, the two digests are equal. No step compares them yet.
[#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583)
plans that gate.

The managed launch reads `chat_template.jinja` from the model folder. A model
that has its template only in a JSON file has no such file. Do not add one
by hand: an added file is not in the upstream tree, so the identity check
fails.

The managed launch does not support such a model. An existing launch can
name a file outside the model folder with `--chat-template`.
Write the template text of the JSON file into that file without changes.
Then the two digests are equal.

The review cannot find an instruction that an operator does not see in the
literals. The behavioral scan covers the model, not the template.

## Results

| Status | Cause |
| --- | --- |
| `passed` | All checks passed. Only this status is clean |
| `findings` | A check found a problem in the files. Examples are a rejected file, a remote code key, invalid JSON and an identity mismatch. A chat template rule that fails is also a finding, other than the reasons that the table marks `unsupported` or `error` |
| `error` | A check did not complete. Causes are a file that the supervisor cannot read (`unreadable`), a failed upstream listing or a file that changed during the check (`files-changed`). A failed template approval (`chat-template-approval-failed`) and a failure of the template checker (`chat-template-check-failed`) are also causes |
| `unsupported` | The checks do not support the model, for example a model without `.safetensors` weights, with a JSON file above the limit or without a chat template |
| `not_scanned` | A check did not run, the files changed after the check, the checker changed, or there is no result |

The supervisor keeps one result for each model in its private state database,
as bounded strict JSON of at most 4 MiB. A result holds these items:

- The model ID, the directory and the source.
- The SHA-256 of each file.
- The status, the coverage, and the outcome and reason code of each check.
  Coverage `complete` means that all checks ran. Coverage `partial` means
  that one or more checks did not run.
- The SHA-256 of the chat template text, when the structural template rules
  passed and no approval failed.
- The review list of the chat template and the approval of an operator,
  see [Review](#review). The review list is the only template text in the
  result. The supervisor does not write it to logs.
- The checker version and the configuration: the hub URL, the file limits
  and the template limits.
- The check time.

Reason codes never hold paths or file content. The result format has
version 2. A result with another positive version is `not_scanned`. A
result without a positive version is not valid.

The supervisor calculates the status again from a new inventory of the files
on disk. An inventory problem that the result does not hold, such as a new
link, gives `not_scanned`. If a file
digest, the directory, the source, the checker version or the configuration
(for example the hub) differs from the result, the status is `not_scanned`.
Thus a new checker version checks each model again.

A canceled check stores no result. The model stays `not_scanned`.

No signatures or keys protect the results. The deployment trusts processes
with the service UID (see [SECURITY.md](../SECURITY.md)). A process with that
UID can forge a result. This is an accepted limit.
