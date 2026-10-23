# Experimental LLM training runner

`gpu-training-runner --job-file /private/training/job.json` implements the
version 1 [finite-job adapter](TRAINING.md). Invoke it through the supervisor's
exact trainer unit, with its bound physical GPU UUID and systemd invocation.
The runner ships without an enabled preset. #453 remains open until runtime,
backend comparison and GPU qualification pass; checkpoint/resume is #454.

The runner accepts the existing strict candidate recipe. Its runtime entrypoint
must be Python 3.12 or newer (the pinned Axolotl minimum), with an inventoried
regular executable at `bin/python3` and packages under
`lib/pythonX.Y/site-packages`. Runtime files must belong to root or the execution
user and must not be group or other writable. Inputs still require complete
inventories, exact recipe and dataset digests, and resource preflight. The output
directory must be empty and private. Preparation never installs dependencies.

Python runs with `-I -S -B`: no ambient Python path, user site, startup `.pth`
execution or bytecode writes. The embedded worker adds only the inventoried
runtime's site-packages. Axolotl's installed `direct_url.json` must identify
noneditable source commit `1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`.
Axolotl entry points are limited by owner, group, name and value to that revision's
two bundled context/expert parallel plugins and four bundled cloud providers
(CoreWeave, Nebius, Modal and Baseten). The guard inspects this installed metadata
without importing or using the providers. Other plugins, providers and model
entry points are rejected before importing Axolotl. Bundled metadata is not
permission to use parallel training or cloud providers.

Before Axolotl starts, each phase checks Torch and CUDA against the bound GPU.
The phase needs a CUDA build of Torch, a usable driver, kernels for the GPU
architecture and native BF16. QLoRA also needs bitsandbytes. An unsupported
combination stops the phase, and training never falls back to CPU. Child output
stays discarded, so use the [runtime check](LLM-TRAINING-QUALIFICATION.md#runtime-check)
to get the actionable reason.

This is a byte/inventory boundary, not a complete approved environment lock.
Python's standard library, OS/native libraries, CUDA and driver compatibility
still require a separately pinned and qualified deployment. Do not treat a
matching source metadata file or a successful fixture as runtime approval.

Every execution gets a new private directory beside its lifecycle envelope,
named `.llm-<invocation>`. Reusing that invocation fails. It contains the generated
configuration, embedded worker, isolated home/cache, bounded current progress and
per-phase receipts. The completion credential never enters the worker's files,
arguments or environment. Child stdout/stderr are discarded because third-party
messages can contain private data. Errors report a failed phase without echoing
input paths, datasets or credentials. Retain private trainer artifacts for diagnosis.

Training uses Axolotl's pinned model/trainer setup and native Trainer APIs. It
must reach exactly the requested optimizer step, pass held-out evaluation with a
finite nonnegative loss, and save its adapter/tokenizer. A separate Python process
then loads the base and saved adapter and checks finite forward output and a
bounded deterministic continuation. No generated text enters lifecycle evidence.
Both phases remain in the trainer unit and therefore under the same admission.

Only after both processes succeed does the runner verify the bounded export,
write version 2 `supervisor-provenance.json` beside it, and atomically publish the fenced
`completed` receipt. The provenance binds source, raw recipe, model/tokenizer
revisions and inventories, runtime inventory/entrypoint, embedded worker,
invocation, exported file hashes and held-out loss; it contains no completion
token. Adapter configuration, weights and saved tokenizer files must be unchanged
across reload. `exportBytes` bounds their combined size. A failed phase never
publishes successful completion. Systemd failure or
explicit recovery still supplies the supervisor's failure/interruption handling.
The supervisor independently verifies cgroup/GPU release and capacity afterward.

Hub/dataset offline modes are enabled; telemetry and reporting are disabled,
and ambient credentials are omitted. These settings are not network isolation.
Qualify the runtime and deployment restrictions before enabling a training
profile. The separate [import preparation tool](LLM-TRAINING-IMPORT.md) validates
artifact identity without registering or activating a serving model. Serving
compatibility and checkpoint resume remain outside this runner.

See [qualification](LLM-TRAINING-QUALIFICATION.md) for dependency candidates,
immutable asset revisions, private host inputs and the real-GPU acceptance gates.

Repository tests use an explicitly identified worker fixture for process,
environment, receipt, failure and provenance behavior. Python guard tests check
source and plugin rejection and atomic progress publication. They also check the
accelerator rules with mocked Torch and CUDA values. They do not load model
weights, exercise GPU kernels or establish real training/reload compatibility.

