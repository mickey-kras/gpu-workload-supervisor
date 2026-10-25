# Model seal

[Documentation](README.md) | [Attestation contract](MODEL-ATTESTATION.md)

`gpu-model-seal` makes model attestation material and checks signed
attestations ([#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585)).
It is a one-shot command. It has no daemon and no listener. Run it in
isolated staging, outside the supervisor. The release archive includes it.
The Debian package does not install it.

The command never receives or stores a signing key. It writes the exact
signing bytes. An offline signer signs them. The command then checks the
detached signature against a pinned public key.

A valid signature records provenance and policy decisions. It does not prove
that the model behavior is safe.

## Seal an artifact

```sh
gpu-model-seal seal -root /staging/model -not-scanned -policy-version 1 -output /private/sealed
gpu-model-seal seal -root /staging/model -scanner-config /private/scanner.json -policy-version 1 -output /private/sealed
gpu-model-seal seal -file /staging/model.gguf -not-scanned -policy-version 1 -output /private/sealed
```

Select one target:

- `-root` makes the `files` form. The command walks the complete root and
  binds each regular file, which includes weights, shards, shard index files,
  tokenizer files, configuration and adapters.
- `-file` makes the single form. It binds the digest of one artifact file.

Seal each root separately. For example, seal a model root and a tokenizer root
as two envelopes. The deployment selects the form, not the envelope.

Select one scan choice:

- `-not-scanned` records an intentionally skipped scan as `not_scanned`. A
  skipped scan is never a pass. The policy of
  [#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583)
  decides if it is acceptable.
- `-scanner-config` runs the configured scanner. See
  [Behavioral scanner](#behavioral-scanner).

The scanner is off by default. The command does not run a scanner without
`-scanner-config`.

`-policy-version` is the policy version that the producer applied. It is
necessary.

`-source-uri` and `-source-revision` are optional. Give them only when you
know them. The command records them as claims. It does not invent a source.

### Inventory rules

The inventory uses the shared rules of the training inventory in
`internal/llmtraining`:

- The root is a clean absolute path without links.
- Links and files that are not regular files make the seal fail.
- A root has 1 to 32768 files and at most 65536 entries.
- Each shard index file, such as `model.safetensors.index.json`, must match
  its inventory digest. Each `weight_map` entry must name an inventoried
  `.safetensors` file.

The attestation path alphabet is narrower than the training alphabet. If a
path has other characters, the command refuses to seal the artifact.

A conversion, a quantization or an adapter composition makes a new artifact.
Seal it again. Its digest is different, so it cannot get an earlier verdict.

### Output

`-output` is a new directory. Its parent must be a private directory of the
current user. The output must be outside the artifact and the scanner runtime.

The command writes all files into a private staging directory beside the
output. Then it renames that directory into place. The rename never replaces
an existing path. Thus a reader sees no directory or the complete set.

| File | Contents |
| --- | --- |
| `envelope.json` | The canonical envelope, as `MarshalEnvelope` gives it |
| `envelope.signing-bytes` | The exact bytes to sign: `gpu-model-attestation/v1`, one line feed and the envelope |
| `report.json` | The scan report. It is absent for `not_scanned` |

The directory has mode `0700`. Each file has mode `0600`.

The command prints one JSON line with the artifact digest and the scan status.
It does not print paths, file content or report detail.

## Sign offline

Sign `envelope.signing-bytes` with a pure Ed25519 signature (RFC 8032) on a
separate signing system. Do not hash the bytes first. Then write the
signature file:

```json
{"version":1,"algorithm":"ed25519","keyId":"<hex SHA-256 of the raw public key>","signature":"<base64 of the 64-byte signature>"}
```

[Signature and key identity](MODEL-ATTESTATION.md#signature-and-key-identity)
gives the rules.

## Verify

```sh
gpu-model-seal verify -root /models/model -envelope /evidence/envelope.json -signature /evidence/signature.json -trust /etc/trust.json -policy /etc/policy.json
```

`verify` walks the target again and calls `attestation.Verify`. With
`-policy`, it also calls `attestation.Decide`. The supervisor gate of #583
uses the same functions. Thus the command and the gate give the same result
for the same inputs.

The trust configuration and the policy must be regular files without links
that other users cannot change. A policy file that is missing, unreadable or
not valid gives `invalid-policy`. The command never uses advisory mode as a
fallback.

The command prints one JSON line with the result, the artifact digest, the key
ID, the scan status and the decision. It exits with status 0 only when the
result is `valid` and the policy, if any, allows the artifact.

## Behavioral scanner

The first scanner adapter is
[Microsoft's LLM Backdoor Scanner](https://github.com/microsoft/llm-backdoor-scanner)
at revision `9d2ef6be06fc034c001051c1b16856af0b8a9ab4`.

A behavioral scan executes the model. Run it only in isolated staging. Do not
give the staging system signing keys or production credentials. "Before first
use" means before approved serving or training, not before the isolated scan.

GPU Supervisor setup does not install the scanner, its runtime or a wrapper.

### Supported artifacts

The adapter supports only these artifacts:

| Item | Supported value |
| --- | --- |
| Form | The `files` form |
| Weights | `.safetensors` only. Pickle, GGUF, ONNX and other weight files are unsupported |
| Architecture | One of `LlamaForCausalLM` (`llama`), `Phi3ForCausalLM` (`phi3`), `Gemma3ForCausalLM` (`gemma3_text`), `Qwen2ForCausalLM` (`qwen2`) in `config.json` |
| Configuration | No `auto_map` and no `quantization_config` |
| Adapters | No `adapter_config.json`. A PEFT adapter needs its base model, which is a different root |
| Tokenizer | `tokenizer_config.json` in the same root |
| Hardware | A CUDA GPU in the staging system |

The scanner revision documents these model families. Support of an
architecture does not mean that the scanner was qualified on every model of
that architecture.

For other artifacts, the command does not run the scanner. It records
`unsupported` with the scanner identity and a report.

### Scanner configuration

The scanner configuration is strict JSON, at most 1 MiB. Other users must not
be able to change the file.

| Field | Rule |
| --- | --- |
| `version` | Integer `1` |
| `adapter` | `llm-backdoor-scanner` |
| `revision` | `9d2ef6be06fc034c001051c1b16856af0b8a9ab4` |
| `runtime.inventory` | Absolute `root` and `files` of the complete staging runtime: interpreter, dependencies, scanner checkout and wrapper |
| `runtime.entrypoint` | Relative path of the wrapper in the runtime inventory |
| `source` | Relative directory of the scanner checkout in the runtime inventory |
| `experiment` | `exp1` or `exp2`, the scanner experiment type |
| `threshold` | The verdict threshold of the scanner, above 0 and at most 1000 |
| `timeoutSeconds` | 60 to 172800 |
| `devices` | Optional. GPU indexes for `CUDA_VISIBLE_DEVICES`, such as `0` or `0,1` |

The runtime inventory must hold the scanner files of the pinned revision. The
command checks the digests of the verdict logic and the stage scripts. The
envelope records the tool `llm-backdoor-scanner`, the revision as the version
and the SHA-256 of the canonical configuration as `configSHA256`.

The thresholds and the experiment type are operator choices. The
configuration digest records them.

### Wrapper contract

The pinned scanner has several stages. The wrapper runs the leakage, motif
extraction and motif search stages of the pinned revision. The command runs
the wrapper with these arguments only:

```text
--model-dir <root> --output-dir <scratch> --experiment <exp1|exp2> --threshold <value>
```

The wrapper writes the verdict file of the motif search stage to
`<scratch>/verdict.json`. The command creates the scratch directory privately
beside the output and removes it after the scan.

### Isolation and bounds

The command does these steps:

1. It checks the complete runtime inventory against the disk.
2. It runs the wrapper without standard input, in its own process group, with
   the scratch directory as the working directory.
3. It gives the wrapper only `PATH=/usr/bin:/bin`, `HOME` and `TMPDIR` set to
   the scratch directory, `LANG=C.UTF-8`, offline settings for model hubs,
   `PYTHONDONTWRITEBYTECODE=1`, `PYTHONNOUSERSITE=1` and the optional
   `CUDA_VISIBLE_DEVICES`. It passes no other environment variable.
4. It stops the process group at the timeout, or when the combined standard
   output and error exceed 64 MiB. It discards that output, because it can
   hold model text.
5. It checks the model inventory against the disk again.
6. It reads `verdict.json` as bounded strict JSON of at most 1 MiB.

The second inventory check finds changes that stay after the scan. It cannot
find a change that the wrapper reverts. Keep the model root private to the
staging user.

An interrupt stops the scan and the command publishes nothing.

The command does not limit disk use, memory or network access of the
wrapper. Use a staging system with these limits, for example a container
without a network and with a bounded scratch file system.

### Verdict and report

The command maps the verdict to the scan status:

| Verdict file | Scan status |
| --- | --- |
| `NOT_SUSPICIOUS` | `passed`, coverage `complete` |
| `SUSPICIOUS` | `findings`, coverage `complete` |
| Wrapper error, timeout or output limit | `error` |
| Changed model or runtime | `error` |
| Missing, malformed or inconsistent verdict | `error` |
| No evaluated candidates | `error`. The scan is not conclusive |

The command checks that the verdict uses the configured experiment and
threshold, that the label agrees with the metric, and that the candidate
counts agree. It accepts at most 16 candidates of at most 4096 bytes and 512
tokens each.

`report.json` holds the artifact digest, the scanner identity, the status, the
coverage, a list of named checks with their outcomes and the verdict. A check
outcome is `passed`, `findings`, `unsupported`, `skipped`, `error` or
`timeout`. The report has no timestamp, so the same inputs give the same
bytes. The envelope binds its SHA-256 as `reportSHA256`.

A findings report can hold recovered trigger candidates, which are model
output. Keep the report protected and separate from ordinary logs. A failed
scan gives no passing approval. A signed `findings` report is evidence. It is
not an approval.

### Test coverage

The tests use a fixture wrapper and fixture verdict files. They cover known
positive and known negative verdicts, reproducible reports, failure,
timeout, output limit, changed files, malformed verdicts and unsupported
artifacts.

The tests do not run the real scanner. It needs a CUDA GPU and model weights.
Qualify the wrapper and the scanner on the staging system with a known
backdoored model and a known clean model before you rely on its verdicts.
