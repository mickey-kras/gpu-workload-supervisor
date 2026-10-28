# Development

[Documentation](README.md) | [Repository](../README.md)

Use the Go version declared in [go.mod](../go.mod) and Node.js 22 on `PATH`.
The Go tests invoke a Node harness to check the setup UI serializer against the
Go validator. Node is a test prerequisite, not a production dependency.
From the repository root:

```sh
go test ./...
go vet ./...
```

CI keeps total statement coverage at 90% and enforces each package's floor in
[.testcoverage.yml](../.testcoverage.yml). New packages need 90%. Existing lower
floors are explicit. Raise the floors as tests improve. Do not lower them to pass a PR.
Run the same checks locally:

```sh
go test -race -coverprofile=coverage.out ./...
go run github.com/vladopajic/go-test-coverage/v2@v2.19.0 --config=.testcoverage.yml
```

See [host qualification](DEPLOYMENT.md#qualify-the-host) for the optional systemd integration suite, its prerequisites and its coverage limits.
Complete that qualification before you enable execution.

## PR validation scope

Only a complete PR diff confined to `README.md` or Markdown files below `docs/`
uses the documentation route. Runtime, UI, workflow, policy, configuration,
dependency and packaging paths, mixed changes and unknown paths use full
validation. Renames must qualify at both their old and new paths. Deleted files
need complete diff metadata. Forks use the same read-only classification.

| Work | Documentation route | Full route |
| --- | --- | --- |
| Native GTK fixtures | Skipped | Required |
| Go race tests, coverage, benchmark, systemd qualification, vet and complexity | Skipped | Required |
| Go formatting, module lock and vulnerability audit | Required | Required |
| Frontend tests, npm audits and policy tests | Required | Required |
| ste-cli and Vale prose checks | Required | Required |
| Gitleaks, Semgrep, Trivy, Aislop, CodeQL and dependency review | Required | Required |
| Snapshot packages and desktop payload and lifecycle qualification | Required | Required |
| Go coverage and fully CI-tested candidate artifacts | Not produced | Produced after their gates succeed |

Documentation ships inside release packages, so its route still builds and
qualifies packages. It does not promise a one-minute run. `quality / checks`
accepts only the classifier-authorized native skip. Every other producer must
succeed. Failures and cancellations remain failures.

Main and release work run
full validation regardless of the scope input. Sonar remains main-only.

The classifier checks live PR head and base identity before and after paginated
file reads. Missing, truncated, changed or failed API evidence selects full
validation. Its log states `Validation mode: full` with a safe fallback reason.
If the classifier job itself fails, quality validation also defaults to full.
Concurrency cancellation stops the remaining quality work.

Rerun the PR workflow after transient API errors. No label or PR-provided input
can force the documentation route. The classifier runs protected bytes from the
trusted base commit. Changes to its allowlist, runtime wrapper or skip wiring
must first reach the trusted guard through the normal policy review process.

## Desktop package CI

The required `quality / checks` job runs native GJS and Gio transport tests.
It extracts the actual amd64 snapshot Debian payload into a disposable runner.
It tests the setup executable as a new OS account, real user systemd and SQLite.

The tests cover:

- Discovery, preview without side effects, and explicit confirmation.
- An interrupted activation after catalog commit and durable forward resume.
- Login reconciliation through the packaged unit and stale preview rejection.
- Backup tuples with checksums and owned integration removal and reapplication.

The tests use an identified NVIDIA command fixture. This gate does not claim a GPU.

A separate dpkg temporary-root test exercises unpack, remove, purge and reinstall
of the unchanged payload and checks preservation of private data. It does not
configure the package or bypass its GNOME 50 dependencies. These checks fail when
prerequisites are absent. They are unconditional parts of the existing gate.
The scripts need a disposable host and refuse existing installed backend paths.

See [releasing](RELEASING.md).

## Documentation language

Use ASD-STE100 Issue 9 for new and changed documentation.
Keep technical meaning, security conditions, numeric limits, commands and identifiers.
Keep exact legal text and quotations unchanged.
Check the applicable writing rules and dictionary entries.

The required `quality / checks` job needs both `docs-ste / ste-cli` and `docs-ste / vale` to succeed.
Both checks run on all CI events, including PRs that change only documentation.
They check repository Markdown, including hidden paths, mixed-case extensions and names with spaces.
The API contracts are Markdown and use the same checks.
This repository has no OpenAPI or Swagger specification.

The discovery step excludes dependency files, downloaded Vale styles and identified legal notices.
It checks new untracked Markdown during local validation and tracked Markdown in CI.
It does not exclude release notes or all generated files.

Do not add linter control comments to Markdown.
The staging step rejects those comments.
The trusted policy guard protects the workflow, linter configuration and vocabulary.

The setup checks both linter archives and the style archive against fixed SHA-256 digests before extraction.
The checks run verified binaries directly.
They do not run `vale sync` or download reviewdog.
Policy tests reject missing jobs, weaker thresholds and changes to protected prose checks.

For local setup, open a shell in the repository root, set
`RUNNER_TEMP=$(mktemp -d)`, and run the three installation steps from
`.github/workflows/docs-ste.yml` in the same shell.

ste-cli and Vale check only part of ASD-STE100.
Neither configured tool checks the full official dictionary.
Successful checks do not certify full Issue 9 compliance.
A complete review must check the official dictionary and applicable writing rules.
