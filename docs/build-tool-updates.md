# Build tool updates

| Pin | Location | Update coverage |
| --- | --- | --- |
| GoReleaser binary | `.github/actions/setup-goreleaser/action.yml` | Weekly/manual `build tool updates` check |
| cosign binary (`cosign-release`) | Same composite action | Weekly/manual check |
| Trivy binary (`version`) | `.github/workflows/release.yml` | Weekly/manual check |
| go-licenses/v2 module | `scripts/third-party-notices.sh` | Weekly/manual check |
| setup-trivy and trivy-action SHA refs | CI/release workflows | Dependabot `github-actions` |
| cosign-installer SHA ref | Nested setup-goreleaser composite action | Manual review; not covered by root-only Dependabot configuration |
| CI Trivy binary default | Pinned trivy-action | Follows the reviewed action update |

Dependabot's existing Go module and GitHub Actions updates do not cover these
shell literals or action inputs. The read-only check uses upstream stable release
metadata to produce a summary, base commit and version-only patch artifact. No
additional bot, write token or automatic merge is used. API errors and malformed
pins fail the check; old upstream releases cannot downgrade a pin.

The existing `github-actions` configuration scans workflows and root action files,
not the nested composite action. Its cosign-installer SHA still needs manual
upstream review with the protected installer digest; this four-binary check does
not propose installer-action SHA changes.

Download the artifact from the workflow run. On its recorded base commit, use
`git apply --check build-tool-updates.patch`, then apply and review the patch in
a normal PR. An empty patch means no directly applicable updates; read the summary
for a go-licenses module-major migration requiring manual changes.

GoReleaser/cosign changes require a separately reviewed installer digest update in
`policy-guard.cjs` using the existing policy-change process. The proposal deliberately
does not authorize that change. Keep the exact signing identity, OIDC issuer,
signature and archive checksum verification, and Go public checksum-database
settings intact. Run the installer tests and all normal PR gates before merging.
The check does not download or execute proposed binaries.
