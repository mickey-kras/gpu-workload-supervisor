# Model attestation contract

[Documentation](README.md) | [Architecture](ARCHITECTURE.md)

This page defines format version 1 of the signed model attestation. The
package `internal/attestation` holds the contract, the verifier and the
fixtures. The producer CLI ([#585](https://github.com/mickey-kras/gpu-workload-supervisor/issues/585))
and the supervisor gate ([#583](https://github.com/mickey-kras/gpu-workload-supervisor/issues/583))
do not exist yet. When they exist, both must use this package.

Signatures attest provenance and policy decisions, not model safety.

## What a signature does not prove

A valid signature proves that a pinned key signed this evidence for these exact
bytes. It does not prove these items:

- The model is safe, correct or free of backdoors.
- The scanner found every problem. Scanners detect only known patterns.
- The source and revision are true. The producer records them as claims.
- The timestamp is correct. The timestamp is a signer claim and has no expiry.
- The runtime loads only the bound files. The caller must make sure of this.

## Files

An attestation is two files:

- An envelope: strict JSON, at most 16 MiB.
- A detached signature: strict JSON, at most 1 KiB.

The readers reject these inputs:

- Input that is not valid UTF-8, truncated input and input that is too large.
- Duplicate keys at any depth, unknown fields and trailing data.
- Field names with a different case, `null` values and empty optional strings.
- Numbers that are not integers.

The decoded input must be equal to its canonical form. Whitespace, key order
and JSON string escapes can change. Names and values cannot change.

## Envelope fields

| Field | Rule |
| --- | --- |
| `version` | Integer `1`. A different positive integer gives `unsupported-version`. |
| `artifact.files` | Map of relative path to lowercase hex SHA-256. Use 1 to 32768 entries. |
| `artifact.sha256` | Lowercase hex SHA-256 of one artifact file. Use this form or `files`, not both. |
| `source.uri` | Optional. Where the producer got the artifact. Maximum 2048 bytes. |
| `source.revision` | Optional. Source revision. Maximum 256 bytes. |
| `scan.status` | `passed`, `findings`, `not_scanned`, `unsupported` or `error`. |
| `scan.scanner` | Object with `tool`, `version` and `configSHA256`. |
| `scan.scanner.tool` | Scanner name. Maximum 256 bytes. |
| `scan.scanner.version` | Scanner version. Maximum 256 bytes. |
| `scan.scanner.configSHA256` | Digest of the effective scanner configuration. |
| `scan.coverage` | `complete` or `partial`. The report gives the detail for each file. |
| `scan.verdict` | The scanner verdict text. Maximum 256 bytes. |
| `scan.reportSHA256` | Lowercase hex SHA-256 of the scanner report. |
| `policyVersion` | Integer from 1 to 2147483647. The policy version that the producer applied. |
| `timestamp` | UTC time in the `2006-01-02T15:04:05Z` form. No fraction and no offset. |

Omit `source` when the source is unknown. If `source` is present, it needs one
or more of its fields. The producer must not invent a source or a revision.

The `files` form binds the complete effective artifact. Include weights,
shards, shard index files, tokenizer files, configuration and adapters. A
shard index file such as `model.safetensors.index.json` is an ordinary entry.

Scan evidence depends on the status:

| Status | `scanner` | `coverage` | `verdict` | `reportSHA256` |
| --- | --- | --- | --- | --- |
| `passed` | Necessary | Necessary | Necessary | Necessary |
| `findings` | Necessary | Necessary | Necessary | Necessary |
| `error` | Necessary | Not permitted | Not permitted | Optional |
| `unsupported` | Necessary | Not permitted | Not permitted | Optional |
| `not_scanned` | Not permitted | Not permitted | Not permitted | Not permitted |

Record a skipped scan as `not_scanned`. A skipped scan is never a pass.

## Paths and text

All strings use printable ASCII from `0x20` to `0x7E`. The reader rejects the
quote character and the backslash. Thus canonical strings need no escapes.

Each path must obey these rules:

- The separator is `/`.
- The path does not start or end with `/`.
- No segment is empty, `.` or `..`.
- The path has a maximum of 512 bytes.

This alphabet is narrower than the alphabet of training inventories. The
producer must record an artifact with other path characters as `unsupported`.

## One envelope for each inventory

Each envelope binds one inventory root. Keep separate roots, such as a model
root and a tokenizer root, in separate envelopes. Use `DecideAll` to combine
their decisions. The most severe status wins in this order:

1. `integrity-failure`
2. `findings-detected`
3. `not-verified`
4. `exempted`
5. `verified`

One rejection rejects the full set. An empty set is a missing attestation.

## Canonical signing bytes

Do these steps to make the signing bytes:

1. Parse and check the envelope with the rules above.
2. Remove each absent optional field. Do not write `null` or empty strings.
3. Sort the members of each object by the bytes of their keys.
4. Write JSON with no whitespace between tokens.
5. Write each string between quotes with no escapes.
6. Write each integer in decimal with no sign and no leading zero.
7. Put the 24 bytes `gpu-model-attestation/v1` and one line feed byte first.

The signing bytes have no line feed at the end. `MarshalEnvelope` gives the
result of steps 1 to 6.

The tests compare steps 3 to 6 with a sorted-key compact JSON encoder. The
integer limit and the ASCII alphabet keep this output equal to the RFC 8785
form. The file `internal/attestation/testdata/passed.signing-bytes` holds a
known answer.

## Signature and key identity

The signature file has these fields:

| Field | Rule |
| --- | --- |
| `version` | Integer `1`. A different positive integer gives `unsupported-version`. |
| `algorithm` | String `ed25519`. The reader rejects other values as malformed. |
| `keyId` | Lowercase hex SHA-256 of the raw 32-byte public key. |
| `signature` | Standard base64 with padding of the 64-byte Ed25519 signature. |

The signature is a pure Ed25519 signature (RFC 8032) over the signing bytes.
The verifier uses Go `crypto/ed25519`. That check is cofactorless and rejects
a signature with S equal to or more than the group order L. The signer works
offline. The producer CLI must never get or store a signing key.

Trusted configuration pins each public key. Write each key as standard base64
with padding of the raw 32 bytes. The verifier accepts 1 to 16 pinned keys.
It rejects these keys:

- Small-order keys. The list is the libsodium `has_small_order` list. The check
  ignores the sign bit.
- Keys with a y coordinate equal to or more than 2^255 - 19.
- The two test-only fixture keys.

A small-order key accepts forged signatures for all messages. The tests show
such a forgery with the identity key.

## Trust configuration

The producer and the gate must read one trust file with `ReadTrustConfig`. The
file is strict JSON, at most 1 MiB:

| Field | Rule |
| --- | --- |
| `version` | Integer `1`. |
| `keys` | List of objects with `keyId` and `publicKey`. Each `keyId` must match its key. |
| `policyVersions` | The supported policy versions. |
| `policy` | Object with `mode`, `exemptions` and `deny`. |

Omit `exemptions` and `deny` when they are empty. `NewVerifierFromConfig`
applies the same key checks as `NewVerifier`.

## Verification results

The verifier does the checks in this order. The first failure gives the result.
It reads the small signature file before the large envelope.

| Order | Result | Cause |
| --- | --- | --- |
| 1 | `malformed` | The verifier, the envelope reader or the signature reader is missing. |
| 2 | `invalid-inventory` | The caller inventory breaks the artifact rules. |
| 3 | `unreadable`, `unsupported-version` or `malformed` | The signature file. |
| 4 | `unknown-key` | No pinned key has the key ID. |
| 5 | `unreadable`, `unsupported-version` or `malformed` | The envelope. |
| 6 | `invalid-signature` | The signature does not match the signing bytes. |
| 7 | `incompatible-policy` | The configuration does not support the policy version. |
| 8 | `artifact-mismatch` | The bound digests are not equal to the inventory on disk. |
| 9 | `valid` | All checks pass. |

The error tells the failure kind. It does not show paths, digests or file
content. Only `Verify` makes a sealed result. `Decide` rejects all other values.

## Policy decision

The policy mode is `advisory` or `enforce`. The configuration must set it.
`Decide` applies `enforce` to every other value, which includes an empty mode.
The configuration loader of #583 must apply the `advisory` default and show it.

A signature is separate from the scan outcome. A signed `findings` report is
evidence. It is not an approval.

| Evidence | `advisory` | `enforce` | Reason |
| --- | --- | --- | --- |
| No attestation | Allow, `not-verified` | Reject, `not-verified` | `attestation-missing` |
| `unreadable` | Allow, `not-verified` | Reject, `not-verified` | `unreadable` |
| `unsupported-version` | Allow, `not-verified` | Reject, `not-verified` | `unsupported-version` |
| Other failed result | Reject, `integrity-failure` | Reject, `integrity-failure` | The result code |
| Denied artifact digest | Reject, `integrity-failure` | Reject, `integrity-failure` | `artifact-denied` |
| `passed` with `complete` coverage | Allow, `verified` | Allow, `verified` | `scan-passed` |
| `findings` | Reject, `findings-detected` | Reject, `findings-detected` | `scan-findings` |
| `error` | Allow, `not-verified` | Reject, `not-verified` | `scan-error` |
| `not_scanned` | Allow, `not-verified` | Reject, `not-verified` | `scan-not-scanned` |
| `unsupported` | Allow, `not-verified` | Reject, `not-verified` | `scan-unsupported` |
| `passed` with `partial` coverage | Allow, `not-verified` | Reject, `not-verified` | `scan-partial-coverage` |

An exemption can change only these rows: `findings`, `not_scanned`,
`unsupported`, and `passed` with `partial` coverage. Then both modes allow the
artifact with the `exempted` status. The reason stays the same. An exempted
artifact is never `verified`.

An exemption has these fields. All must match, except `reason`:

| Field | Rule |
| --- | --- |
| `artifact` | The artifact digest. |
| `policyVersion` | The policy version of the envelope. |
| `kind` | `findings`, `not-scanned`, `unsupported` or `partial-coverage`. |
| `reason` | Necessary text. Maximum 256 bytes. |

The artifact digest is the lowercase hex SHA-256 of `gpu-model-artifact/v1`,
a line feed and the canonical JSON of the `artifact` object. The policy
rejects two exemptions with the same artifact, policy version and kind. The
setup journal of #583 must record who added an exemption and when.

An exemption never changes these outcomes:

- A missing, unreadable or `unsupported-version` attestation.
- An integrity failure, which includes a forged signature or an artifact mismatch.
- A denied artifact digest.
- An `error` result.

## Revocation

Put an artifact digest in `deny` to reject that artifact. The deny check comes
before the scan outcome and before exemptions.

Remove a key from the configuration to reject all evidence that the key
signed. Remove a policy version to reject all evidence for that version. Then
the producer must attest the artifacts again.

To rotate a key, do these steps:

1. Pin the new key next to the old key.
2. Sign new evidence with the new key.
3. Sign the old evidence again with the new key, or replace that evidence.
4. Remove the old key from the configuration.

The verifier has no mutable allowlist, no daemon and no listener.

## Caller obligations

The gate and the producer must obey these rules:

- Make the on-disk inventory from a full walk of the effective artifact.
  Never make it from the envelope.
- Let the deployment select the `files` form or the single form. The envelope
  must not select it.
- Reject symbolic links and files that are not regular files.
- Calculate the digests of the bytes that the runtime loads. Use a held file
  handle or a read-only copy.
- Prevent changes between the check and the load.
- Treat an envelope without a signature as an integrity failure. Do the same
  for a signature without an envelope. Only the absence of both files is a
  missing attestation.

## Fixtures

The fixtures are in `internal/attestation/testdata`. Test-only keys sign them.
The tests make the keys from fixed public seeds. The repository has no private
key material. `NewVerifier` rejects the test-only keys.

The file `artifact-digests.json` holds artifact digests from a different
encoder. The fixture command does not write it.

To make the other fixtures again, run this command:

```sh
go test ./internal/attestation -run TestCommittedFixturesMatchDeterministicGeneration -update
```
