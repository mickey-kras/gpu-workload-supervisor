# LLM artifact import preparation

`gpu-training-import` validates a local PEFT export against the original recipe
and writes a new private preparation plan. It does not copy artifacts, convert
weights, register a serving model, replace a model, or activate a workload.

```sh
gpu-training-import -format peft -recipe /private/recipe.json -output /private/new-import-plan.json
```

Keep the destination parent private. Publication never replaces an existing
file. The plan contains `status: "unqualified"` and `servingQualified: false`.
`peft` identifies the artifact format, not a serving runtime.

The command rejects vLLM,
llama.cpp, GGUF and other target values. A PEFT reload does not
prove compatibility with any of those runtimes. Before you enable serving import for one of those runtimes, qualify the version and a supported base and adapter combination.
Also collect evidence of supervised serving and handoff.

The runner writes provenance version 2 only after successful training,
evaluation and separate-process reload. It binds the original recipe bytes, model and tokenizer revisions and complete inventory identities.
It also binds the runtime inventory and entrypoint, embedded worker, invocation, held-out loss and exported files.
The runner checks adapter weights, adapter configuration and saved tokenizer files
before and after reload. Added, removed or changed participating files prevent
completion. `exportBytes` now bounds their combined size, not only adapter weights.

Supported exports contain one `adapter_model.safetensors`, an
`adapter_config.json`, a `tokenizer_config.json`, and at least one of
`tokenizer.json` or `tokenizer.model`. The optional bound files are
`special_tokens_map.json`, `added_tokens.json`, `generation_config.json`,
`config.json`, `vocab.json`, `vocab.txt`, `merges.txt` and `chat_template.jinja`.
Only these named files belong to the import plan. Checkpoints, caches, training
arguments, README files and other training outputs are not import payloads.
The command does not support sharded adapters, pickle adapters, template directories or configuration
redirects to other code or files. The adapter must declare LoRA,
causal language modeling and the exact local base path from the recipe.

Preparation rechecks the original model, tokenizer and runtime inventories,
the full recipe digest and every exported file. It requires version 2 provenance
from the same worker implementation and source revision. It rejects old weight-only
provenance. Use the same supervisor release for training and import
preparation.

Both materialization and import hash the complete model inventory,
so large models need a full read of their weights at each step.
The plan is a snapshot: consumers must revalidate before copying or loading its
listed files. It is neither a signature nor proof against a trusted owner who
rewrites both data and metadata. Private paths and inventories remain private.

This command does not import or execute a runtime. The plan excludes dataset contents and
completion credentials. A plan does not prove
supervisor finalization, GPU release, environment approval or serving fitness.
#453 remains open until those host qualification gates pass.
