# Model attestation contract

[Documentation](README.md) | [Architecture](ARCHITECTURE.md)

This page defines format version 1 of the signed model attestation. The
producer CLI ([#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585))
and the supervisor gate ([#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583))
use the same package, `internal/attestation`, and the same fixtures.

Signatures attest provenance and policy decisions, not model safety.

## What a signature does not prove

A valid signature proves that a pinned key signed this evidence for these exact
bytes. It does not prove these items:

- The model is safe, correct or free of backdoors.
- The scanner found every problem. Scanners detect only known patterns.
- The source and revision are true. The producer records them as claims.
- The timestamp is correct. The timestamp is a signer claim and has no expiry.
- The runtime loads only the bound files. The supervisor gate must check this.

## Files

An attestation is two files:

- An envelope: strict JSON, at most 16 MiB.
- A detached signature: strict JSON, at most 1 KiB.

Both readers reject these inputs:

- Input that is not valid UTF-8, input with missing bytes and input that is too large.
- Duplicate keys at any depth, unknown fields and trailing data.
- Field names with a different case, `null` values and empty optional strings.
- Numbers that are not integers.

The decoded input must be equal to its canonical form. Whitespace, key order
and JSON string escapes can change. Names and values cannot change.

## Envelope fields

| Field | Rule |
| --- | --- |
| `version` | Integer `1`. The reader rejects other values as malformed. |
| `artifact.files` | Map of relative path to lowercase hex SHA-256. Use 1 to 32768 entries. |
| `artifact.sha256` | Lowercase hex SHA-256 of one artifact file. Use this form or `files`, not both. |
| `source.uri` | Optional. Where the producer got the artifact. Maximum 2048 bytes. |
| `source.revision` | Optional. Source revision. Maximum 256 bytes. |
| `scan.status` | `passed`, `findings`, `not_scanned`, `unsupported` or `error`. |
| `scan.scanner` | Object with `tool`, `version` and `configSHA256`. `configSHA256` is the digest of the effective scanner configuration. |
| `scan.coverage` | `complete` or `partial`. The report gives the per-file detail. |
| `scan.verdict` | The scanner verdict text. Maximum 256 bytes. |
| `scan.reportSHA256` | Lowercase hex SHA-256 of the scanner report. |
| `policyVersion` | Positive integer. The policy version that the producer applied. |
| `timestamp` | UTC time in the `2006-01-02T15:04:05Z` form. No fraction and no offset. |

Omit `source` when the source is unknown. If `source` is present, it needs one
or more of its fields. The producer must not invent a source or a revision.

The `files` form binds the complete effective artifact. Include weights,
shards, shard index files, tokenizer files, configuration and adapters. A
shard index file such as `model.safetensors.index.json` is an ordinary entry.

Each path must obey these rules:

- The path is relative and clean. `filepath.Clean` does not change it.
- The path does not contain `..` and does not start with `/`.
- The path is not `.` and has no empty segment.
- The path has a maximum of 512 bytes.

All strings use printable ASCII from `0x20` to `0x7E`. The reader rejects the
quote character and the backslash. Thus canonical strings need no escapes.

Scan evidence depends on the status:

| Status | `scanner` | `coverage` | `verdict` | `reportSHA256` |
| --- | --- | --- | --- | --- |
| `passed` | Necessary | Necessary | Necessary | Necessary |
| `findings` | Necessary | Necessary | Necessary | Necessary |
| `error` | Necessary | Not permitted | Not permitted | Optional |
| `unsupported` | Necessary | Not permitted | Not permitted | Optional |
| `not_scanned` | Not permitted | Not permitted | Not permitted | Not permitted |

Record a skipped scan as `not_scanned`. A skipped scan is never a pass.

## Canonical signing bytes

Do these steps to make the signing bytes:

1. Parse and check the envelope with the rules above.
2. Remove each absent optional field. Do not write `null` or empty strings.
3. Sort the members of each object by the bytes of their keys.
4. Write JSON with no whitespace between tokens.
5. Write each string between quotes with no escapes.
6. Write each integer in decimal with no sign and no leading zero.
7. Put the 24 bytes `gpu-model-attestation/v1` and one line feed byte first.

Steps 3 to 6 give the RFC 8785 form for this restricted alphabet. Thus a JSON
Canonicalization Scheme library gives the same bytes. The file
`internal/attestation/testdata/passed.signing-bytes` holds a known answer.

## Signature and key identity

The signature file has these fields:

| Field | Rule |
| --- | --- |
| `version` | Integer `1`. |
| `algorithm` | String `ed25519`. The reader rejects other values as malformed. |
| `keyId` | Lowercase hex SHA-256 of the raw 32-byte public key. |
| `signature` | Standard base64 with padding of the 64-byte Ed25519 signature. |

The signature is a pure Ed25519 signature (RFC 8032) over the canonical
signing bytes. The signer works offline. The producer CLI never gets or stores
a signing key.

Trusted configuration pins each public key. Write each key as standard base64
with padding of the raw 32 bytes. The verifier accepts 1 to 16 pinned keys.
It rejects a key ID that has no pinned key.

To rotate a key, do these steps:

1. Pin the new key next to the old key.
2. Sign new evidence with the new key.
3. Sign the old evidence again with the new key, or replace that evidence.
4. Remove the old key from the configuration.

The verifier has no mutable allowlist, no daemon and no listener.

## Verification results

The verifier does the checks in this order. The first failure gives the result.

| Result | Cause |
| --- | --- |
| `malformed` | The envelope or the signature file breaks a rule on this page. |
| `unknown-key` | No pinned key has the key ID. |
| `invalid-signature` | The signature does not match the canonical signing bytes. |
| `incompatible-policy` | The configuration does not support the policy version. |
| `artifact-mismatch` | The bound digests are not equal to the inventory on disk. |
| `valid` | All checks pass. |

The caller gives the inventory that it found on disk. An extra file, a missing
file, a renamed file or changed bytes give `artifact-mismatch`. Thus evidence
copied to different bytes fails, even with a valid signature.

## Policy decision

The policy mode is `advisory` or `enforce`. The default is `advisory`. An
unknown mode acts as `enforce`.

A signature is separate from the scan outcome. A signed `findings` report is
evidence. It is not an approval.

| Evidence | `advisory` | `enforce` |
| --- | --- | --- |
| No attestation | Allow, `not-verified` | Reject, `not-verified` |
| Any result other than `valid` | Reject, `integrity-failure` | Reject, `integrity-failure` |
| `passed` with `complete` coverage | Allow, `verified` | Allow, `verified` |
| `findings` | Reject, `findings-detected` | Reject, `findings-detected` |
| `error` | Allow, `not-verified` | Reject, `not-verified` |
| `not_scanned` | Allow, `not-verified` | Reject, `not-verified` |
| `unsupported` | Allow, `not-verified` | Reject, `not-verified` |
| `passed` with `partial` coverage | Allow, `not-verified` | Reject, `not-verified` |

An exemption changes only the last three rows. Then both modes allow the
artifact with the `exempted` status. An exemption has two fields:

- `artifact`: the artifact digest.
- `policyVersion`: the policy version of the envelope.

Both fields must match. The artifact digest is the lowercase hex SHA-256 of
`gpu-model-artifact/v1`, a line feed and the canonical JSON of the `artifact`
object.

An exemption never changes these outcomes:

- A missing attestation in `enforce` mode.
- An integrity failure, which includes a forged signature or an artifact mismatch.
- A `findings` result or an `error` result.

## Fixtures

The fixtures are in `internal/attestation/testdata`. Test-only keys sign them.
The tests make the keys from fixed public seeds. The repository has no private
key material. Never pin the test-only keys outside tests.

To make the fixtures again, run this command:

```sh
go test ./internal/attestation -run TestCommittedFixtures -update
```
