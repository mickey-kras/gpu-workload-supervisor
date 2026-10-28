# Experimental LLM training runner

`gpu-training-runner --job-file /private/training/job.json` implements the
version 1 [finite-job adapter](TRAINING.md). Invoke it through the supervisor's
exact trainer unit, with its bound physical GPU UUID and systemd invocation.
The runner ships without an enabled preset. #453 remains open until runtime,
backend comparison and GPU qualification pass. Checkpoint and resume is #454.

The runner accepts the existing strict candidate recipe. Its runtime entrypoint
must be Python 3.12 or newer (the pinned Axolotl minimum), with an inventoried
regular executable at `bin/python3` and packages under
`lib/pythonX.Y/site-packages`. Runtime files must belong to root or the
execution user and must not be group or other writable. Inputs still need
complete inventories, exact recipe and dataset digests, and resource preflight.
The output directory must be empty and private. Preparation never installs
dependencies.

Python runs with `-I -S -B`: no ambient Python path, user site, startup `.pth`
execution or bytecode writes. The embedded worker adds only the inventoried
runtime's site-packages. Axolotl's installed `direct_url.json` must identify
noneditable source commit `1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1`.

The guard limits Axolotl entry points by owner, group, name and value. It
permits only that revision's two bundled context and expert parallel plugins
and four bundled cloud providers (CoreWeave, Nebius, Modal and Baseten). The
guard inspects this installed metadata without importing or using the
providers. Before
importing Axolotl, the guard rejects other plugins, providers and model entry
points. Bundled metadata is not permission to use parallel training or cloud
providers.

Before training starts, each phase checks Torch and CUDA against the bound GPU.
The phase needs a CUDA build of Torch, a usable driver, kernels for the GPU
architecture and native BF16. QLoRA additionally needs bitsandbytes. The configuration
value `load_in_4bit` must be a boolean. An unsupported combination stops the
phase, and training never falls back to CPU.

The runner discards the output of the worker process. To see the reason, run
the [runtime check](LLM-TRAINING-QUALIFICATION.md#runtime-check).

This is a byte and inventory boundary, not a complete approved environment
lock. Python's standard library, OS and native libraries, and CUDA and driver
compatibility still need pinned and qualified provenance. The check rejects
incompatible combinations but does not pin them. A matching source metadata
file or a successful fixture is not runtime approval.

Every execution gets a new private directory beside its lifecycle envelope,
named `.llm-<invocation>`. Reusing that invocation fails. It contains the
generated configuration, embedded worker, isolated home and cache, bounded
current progress, per-phase receipts and a best-effort [resource peaks
file](#resource-peaks). The completion credential never enters the worker's
files, arguments or environment.

The runner discards child stdout and stderr because third-party messages can
contain private data. Errors report a failed phase without echoing input paths,
datasets or credentials. Keep private trainer artifacts for diagnosis.

Training uses Axolotl's pinned model and trainer setup and native Trainer APIs.
It must reach exactly the requested optimizer step, pass held-out evaluation
with a finite nonnegative loss, and save its adapter and tokenizer. A separate
Python process then loads the base and saved adapter and checks finite forward
output and a bounded deterministic continuation. No generated text enters
lifecycle evidence. Both phases stay in the trainer unit and therefore under
the same admission.

Only after both processes succeed does the runner check the bounded export,
write version 2 `supervisor-provenance.json` beside it, and atomically publish
the fenced `completed` receipt. The provenance binds source, raw recipe, model
and tokenizer revisions and inventories, runtime inventory and entrypoint,
embedded worker, invocation, exported file hashes and held-out loss. It
contains no completion token. Adapter configuration, weights and saved
tokenizer files must stay unchanged across reload.

`exportBytes` bounds their combined size. A failed phase never publishes
successful completion. Systemd failure or explicit recovery still supplies the
supervisor's failure or interruption handling. The supervisor independently
verifies cgroup and GPU release and capacity afterward.

The runner enables Hub and dataset offline modes and disables telemetry and
reporting. It omits ambient credentials. These settings are not network
isolation. Qualify the runtime and deployment restrictions before enabling a
training profile. The separate [tool for import
preparation](LLM-TRAINING-IMPORT.md) validates artifact identity without
registering or activating a serving model. Serving compatibility and checkpoint
resume stay outside this runner.

See [qualification](LLM-TRAINING-QUALIFICATION.md) for dependency candidates,
immutable asset revisions, private host inputs and the real-GPU acceptance
gates.

Repository tests use an explicitly identified worker fixture for process,
environment, receipt, failure and provenance behavior. Python guard tests check
source and plugin rejection and atomic progress publication, plus the
accelerator rules with mocked Torch and CUDA values. They do not load model
weights, exercise GPU kernels or prove real training and reload compatibility.

## Resource peaks

Do not trust `peaks.json`. It is best-effort diagnostic data. Code in the
trainer unit can write or change it at any time, with or without a terminal
receipt. A failed job has no receipt by design. Use the file only as an
unverified hint. For qualification, see [the source
order](LLM-TRAINING-QUALIFICATION.md#private-host-inputs-and-evidence).

After creating the private execution directory `.llm-<invocation>` beside the
lifecycle envelope, the runner writes `peaks.json` in it when the job completes
or fails. It writes no file when:

- Validation fails before the runner creates that directory.
- A signal the runner cannot handle stops it, for example an OOM kill or
  `SIGKILL`.

The runner takes one final sample per job, the cgroup peak included. A
finished run takes it before its final input checks. After the checks, the
runner publishes the provenance and then writes the receipt. A run that fails
before its final input checks takes the final sample after the failure. A run
that fails during or after those checks keeps the final sample taken before
them.

The runner writes the file once, after the outcome is final: after the
`completed` receipt or after the failure. A failed write never changes the
outcome. The terminal receipt, not the outcome in `peaks.json`, is the
authoritative outcome. The file is one JSON object of at most a few hundred
bytes. It contains no paths, dataset contents or credentials.

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

A `null` value means the runner did not measure the value. The worker samples
GPU memory at each optimizer step, after held-out evaluation, after the save
and after reload. Each sample is the Torch allocator peak of the worker. It
excludes memory outside the allocator, such as the CUDA context, so it is a
lower bound for the trainer. Other processes on the GPU do not change it.

When a phase fails with a CUDA out-of-memory error, the worker tries to write
one last sample. Other errors make no CUDA call. A killed worker writes none.

A failed sample never fails a phase. Storage samples can miss a short peak, so
the storage value is a lower bound.

The storage sample includes retained checkpoints, a save in progress, the cache
and the export. One walk stops after 65536 entries or 10 seconds. It also stops
at an unreadable directory, at the end of a phase and at a stop signal. A walk
that stops early gives no value, because a partial total is not a usable
capacity value. If no walk finishes, `storagePeakBytes` is `null`.

The walk opens each directory from its parent directory without following
links, so a link that replaces a directory cannot redirect it. It does not
cross into another mount. A blocked filesystem call can hold one walk. At the
end of each phase, the runner waits at most 1 second for a walk in progress. It
waits at most 11 seconds for the final sample: the 10-second walk deadline plus
1 second. After that, the runner drops the walk result.
