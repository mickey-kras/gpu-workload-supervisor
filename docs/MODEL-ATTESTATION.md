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

The contract uses four strict JSON files:

| File | Contents | Maximum size | Reader |
| --- | --- | --- | --- |
| Envelope | The evidence for one artifact | 16 MiB | `Verify` |
| Signature | The detached signature of one envelope | 1 KiB | `Verify` |
| Trust configuration | Pinned keys and supported policy versions | 64 KiB | `ReadTrustConfig` |
| Policy | Mode, exemptions and denials | 1 MiB | `ReadPolicy` |

The readers reject these inputs:

- Input that is not valid UTF-8, truncated input and input that is too large.
- Duplicate keys at any depth, unknown fields and trailing data.
- Field names with a different case, `null` values and empty optional strings.
- Numbers that are not integers.

The decoded input must be equal to its canonical form. Whitespace, key order
and JSON string escapes can change. Names and values cannot change.

Each file has a `version` member. A reader gives `unsupported-version` when the
file is well-formed JSON and `version` is a different positive integer in int64
range. The reader does this check before the field checks.

A `version` below 1 is not a different format. The reader rejects that file
as malformed. For the trust configuration, the error is `ErrInvalidTrust`. For
the policy, the error is `ErrInvalidPolicy`.

## Envelope fields

| Field | Rule |
| --- | --- |
| `version` | Integer `1`. |
| `artifact.files` | Map of relative path to lowercase hex SHA-256. Use 1 to 32768 entries. |
| `artifact.sha256` | Lowercase hex SHA-256 of one artifact file. Use this form or `files`, not both. |
| `source.uri` | Optional. Where the producer got the artifact. Maximum 2048 bytes. |
| `source.revision` | Optional. Source revision. Maximum 256 bytes. |
| `scan.status` | `passed`, `findings`, `not_scanned`, `unsupported` or `error`. |
| `scan.scanner` | Object with the three fields below. |
| `scan.scanner.tool` | Necessary, not empty. Scanner name. Maximum 256 bytes. |
| `scan.scanner.version` | Necessary, not empty. Scanner version. Maximum 256 bytes. |
| `scan.scanner.configSHA256` | Necessary. Lowercase hex SHA-256 of the effective scanner configuration. |
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

Record a skipped scan as `not_scanned`. A skipped scan is never a pass. The
`unsupported` status means that the scanner does not support the format or
the architecture of the artifact.

## Paths and text

All decoded strings use printable ASCII from `0x20` to `0x7E`. The reader
rejects a decoded quote character and a decoded backslash. Thus canonical
strings need no escapes.

Each path must obey these rules:

- The separator is `/`.
- The path does not start or end with `/`.
- No segment is empty, `.` or `..`.
- The path has a maximum of 512 bytes.

This alphabet is narrower than the alphabet of training inventories. An
envelope cannot bind an artifact with other path characters, so the producer
must refuse to seal it. When no evidence files exist, `Decide` gives
`not-verified` with the reason `invalid-inventory`. Advisory mode allows that
artifact, and enforce mode rejects it. When evidence files exist, `Verify`
gives `invalid-inventory`, which is an integrity failure.

## One envelope for each inventory

Each envelope binds one inventory root. Keep separate roots, such as a model
root and a tokenizer root, in separate envelopes. Give `DecideAll` one
`Root{Artifact, Verification}` value for each necessary root.
`Root.Artifact` must be the same gate inventory that you gave to `Verify`. Use
a nil `Verification` for a root without an attestation. The most severe status
wins in this order:

1. `integrity-failure`
2. `findings-detected`
3. `not-verified`
4. `exempted`
5. `verified`

If two entries have the same status, the first entry wins. One rejection
rejects the full set. An empty set is a missing attestation.

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
| `version` | Integer `1`. |
| `algorithm` | String `ed25519`. The reader rejects other values as malformed. |
| `keyId` | Lowercase hex SHA-256 of the raw 32-byte public key. |
| `signature` | Standard base64 with padding of the 64-byte Ed25519 signature. |

The signature is a pure Ed25519 signature (RFC 8032) over the signing bytes.
The verifier uses Go `crypto/ed25519`. That check is cofactorless and rejects
a signature with S equal to or more than the group order L. The signer works
offline. The producer CLI must never get or store a signing key.

The verifier rejects these keys:

- Small-order keys. The list is the libsodium `has_small_order` list. The check
  ignores the sign bit.
- Keys with a y coordinate equal to or more than 2^255 - 19.
- The two test-only fixture keys. The check ignores the sign bit.

A small-order key accepts forged signatures. The tests show a forgery for each
entry in the list.

## Trust configuration

The trust configuration holds signer trust only. It does not hold the policy.

| Field | Rule |
| --- | --- |
| `version` | Integer `1`. |
| `keys` | 1 to 16 objects with `keyId` and `publicKey`. Not `null`. |
| `keys[].publicKey` | Standard base64 with padding of a raw 32-byte Ed25519 key. Each key must be unique. |
| `keys[].keyId` | Lowercase hex SHA-256 of that key. |
| `policyVersions` | 1 or more unique integers from 1 to 2147483647. Not `null`. |

`ReadTrustConfig` and `NewVerifier` use the same checks. Thus the reader
accepts only configurations that the verifier accepts.

## Policy file

The policy is a separate file. The setup of #583 and its rollback journal keep
it. A consumer that stores a policy keeps a `*Policy` value with `omitempty`.

| Field | Rule |
| --- | --- |
| `version` | Integer `1`. |
| `mode` | `advisory` or `enforce`. Necessary. |
| `exemptions` | Optional list. Omit it when it is empty. |
| `deny` | Optional list of unique lowercase hex digests of artifacts or files. Omit it when it is empty. |

Each exemption has these fields:

| Field | Rule |
| --- | --- |
| `artifact` | The artifact digest in lowercase hex. |
| `policyVersion` | Integer from 1 to 2147483647. |
| `kind` | `not_scanned`, `unsupported`, `findings` or `partial_coverage`. |
| `reportSHA256` | Necessary for `findings` and `partial_coverage`. Not permitted for the other kinds. |
| `reason` | Necessary text. Maximum 256 bytes. |

The policy rejects two exemptions with the same scope. The scope is all fields
except `reason`. The setup journal of #583 must record who added an exemption
and when. Exemptions for uncovered execution paths and for missing
attestations belong to the gate coverage configuration of #583. They are not
part of this policy.

The artifact digest is the lowercase hex SHA-256 of `gpu-model-artifact/v1`,
a line feed and the canonical JSON of the `artifact` object.
`Artifact.Digest()` calculates it. `Verification.ArtifactDigest()` gives the
digest of a valid verification. The producer CLI and the UI show this value.

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

An error names the input and the failure kind. For a read failure, it names
the class: permission, timeout or other. It does not show paths, digests,
file content or the text of the reader error. Only `Verify` makes a sealed
result. `Decide` rejects all other values.

## Policy decision

The call is `Decide(policy, verifier, onDisk, verification)`. `DecideAll`
takes the policy, the verifier and the roots. Each call checks the policy
first. An invalid policy gives `integrity-failure` with the reason
`invalid-policy` in both modes, also for an empty set of roots.

Then `Decide` checks the deny list against the gate inventory. It rejects a
denied digest before it looks at the evidence. Thus a deleted or changed
evidence file cannot admit a denied artifact.

`verifier` is the current verifier. A verification from a different verifier
gives `integrity-failure` with the reason `stale-verification`. A nil verifier
with evidence gives the same result. A trust change makes a new verifier, so
evidence that a removed key signed cannot pass.

The mode is `advisory` or `enforce`. A different mode makes the policy invalid,
and `Decide` gives `invalid-policy`. The files have no default. A gate with no
policy file uses `DefaultPolicy()`, which is advisory mode in memory. Setup
writes `"mode": "advisory"` when the operator selects nothing, and shows that
value.

Advisory mode is not a security boundary against a person who can change or
delete evidence files. Only enforce mode is a security boundary.

A signature is separate from the scan outcome. A signed `findings` report is
evidence. It is not an approval.

| Evidence | `advisory` | `enforce` | Reason |
| --- | --- | --- | --- |
| Policy not valid, any evidence | Reject, `integrity-failure` | Reject, `integrity-failure` | `invalid-policy` |
| Denied digest, any evidence | Reject, `integrity-failure` | Reject, `integrity-failure` | `artifact-denied` |
| Evidence from a different verifier | Reject, `integrity-failure` | Reject, `integrity-failure` | `stale-verification` |
| Gate inventory not valid, evidence present | Reject, `integrity-failure` | Reject, `integrity-failure` | `invalid-inventory` |
| Gate inventory not valid, no evidence | Allow, `not-verified` | Reject, `not-verified` | `invalid-inventory` |
| No attestation | Allow, `not-verified` | Reject, `not-verified` | `attestation-missing` |
| `unreadable` | Allow, `not-verified` | Reject, `not-verified` | `unreadable` |
| `unsupported-version` | Allow, `not-verified` | Reject, `not-verified` | `unsupported-version` |
| Other failed result | Reject, `integrity-failure` | Reject, `integrity-failure` | The result code |
| Valid evidence for a different gate inventory | Reject, `integrity-failure` | Reject, `integrity-failure` | `artifact-mismatch` |
| `passed` with `complete` coverage | Allow, `verified` | Allow, `verified` | `scan-passed` |
| `findings` | Reject, `findings-detected` | Reject, `findings-detected` | `scan-findings` |
| `error` | Allow, `not-verified` | Reject, `not-verified` | `scan-error` |
| `not_scanned` | Allow, `not-verified` | Reject, `not-verified` | `scan-not-scanned` |
| `unsupported` | Allow, `not-verified` | Reject, `not-verified` | `scan-unsupported` |
| `passed` with `partial` coverage | Allow, `not-verified` | Reject, `not-verified` | `scan-partial-coverage` |

Advisory mode allows `unreadable` and `unsupported-version` as `not-verified`.
These results do not prove tampering, and the deny check applies first.

An exemption can change only these rows: `findings`, `not_scanned`,
`unsupported`, and `passed` with `partial` coverage. The artifact digest,
the policy version and the kind must match. For `findings` and
`partial_coverage`, the report digest must also match. Then both modes allow
the artifact with the `exempted` status. The reason stays the same. An
exempted artifact is never `verified`.

An exemption never changes these outcomes:

- A missing, unreadable or `unsupported-version` attestation.
- An integrity failure, which includes a forged signature or an artifact mismatch.
- A denied artifact digest.
- An `error` result.

The gate calculates a decision again for each admission. Do not keep a
decision.

## Reason codes

| Reason | Status | Cause |
| --- | --- | --- |
| `invalid-policy` | `integrity-failure` | The policy breaks the policy rules. |
| `artifact-denied` | `integrity-failure` | The policy denies the inventory digest or a file digest. |
| `invalid-inventory` | `integrity-failure` or `not-verified` | The gate inventory breaks the artifact rules. Without evidence, the status is `not-verified`. |
| `attestation-missing` | `not-verified` | No attestation exists for the root. |
| `unreadable` | `not-verified` | A reader failed. |
| `unsupported-version` | `not-verified` | A file has a different format version. |
| `malformed` | `integrity-failure` | A file breaks a rule on this page. |
| `unknown-key` | `integrity-failure` | No pinned key has the key ID. |
| `invalid-signature` | `integrity-failure` | The signature does not match. |
| `incompatible-policy` | `integrity-failure` | The trust configuration does not support the policy version. |
| `artifact-mismatch` | `integrity-failure` | The evidence binds different bytes. |
| `unsealed-verification` | `integrity-failure` | The verification did not come from `Verify`, or it holds an unknown result. |
| `stale-verification` | `integrity-failure` | A different verifier made the verification. Check the evidence again. |
| `scan-passed` | `verified` | A complete scan passed. |
| `scan-findings` | `findings-detected` or `exempted` | The scanner reported findings. |
| `scan-error` | `not-verified` | The scan failed. |
| `scan-not-scanned` | `not-verified` or `exempted` | The scan did not run. |
| `scan-unsupported` | `not-verified` or `exempted` | The scanner does not support the artifact. |
| `scan-partial-coverage` | `not-verified` or `exempted` | The scan did not cover all files. |

## Revocation

Put a digest in `deny` to reject exact bytes. One `deny` list holds artifact
digests and file digests. A deny entry matches these digests:

- The artifact digest of the gate inventory.
- The digest of each file in the inventory, and of a single artifact file.
- The single-form artifact digest of each file. Thus a denial of a single-file
  model also blocks the same bytes in a directory.

Use a file digest to block weights in a copy that has other files or other
names. A file digest matches even when the inventory breaks the artifact
rules. The deny check comes after the policy check and before all other checks
in `Decide`.

A file digest blocks every artifact that contains those bytes. A shared
tokenizer file or an empty file can occur in many models. Thus put only
digests of distinctive bytes, such as weights, in the deny list.

Changed bytes have a different digest, so a deny entry cannot match them. Only
enforce mode makes revocation binding, because it rejects bytes without valid
evidence.

Remove a key from the trust configuration to reject all evidence that the key
signed. Remove a policy version to reject all evidence for that version. Then
the producer must attest the artifacts again.

To rotate a key, do these steps:

1. Pin the new key next to the old key.
2. Sign new evidence with the new key.
3. Sign the old evidence again with the new key, or replace that evidence.
4. Remove the old key from the trust configuration.

The verifier has no mutable allowlist, no daemon and no listener.

## Caller obligations

The gate and the producer must obey these rules:

- Make the on-disk inventory from a full walk of the effective artifact.
  Never make it from the envelope.
- Give the same inventory to `Verify` and to `Decide`.
- Check the evidence again after each trust change. `Decide` rejects a
  verification from an old verifier.
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
