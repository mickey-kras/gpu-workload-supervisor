# Development

[Documentation](README.md) | [Repository](../README.md)

Use the Go version declared in [go.mod](../go.mod). From the repository root:

```sh
go test ./...
go vet ./...
```

CI keeps total statement coverage at 90% and enforces each package's floor in
[.testcoverage.yml](../.testcoverage.yml). New packages need 90%; existing lower
floors are explicit. Raise floors as tests improve; do not lower them to pass a PR.
Run the same checks locally:

```sh
go test -race -coverprofile=coverage.out ./...
go run github.com/vladopajic/go-test-coverage/v2@v2.19.0 --config=.testcoverage.yml
```

The optional systemd integration suite, its prerequisites and its coverage limits are documented under [host qualification](DEPLOYMENT.md#qualify-the-host); complete that qualification before enabling execution.

## Desktop package CI

The required `quality / checks` job also runs native GJS/Gio transport tests,
then extracts the actual amd64 snapshot Debian payload into a disposable runner
and tests its setup executable as a newly created OS account with real user
systemd and SQLite. This covers discovery, side-effect-free preview, explicit
confirmation, an interrupted activation after catalog commit, durable forward
resume, login reconciliation through the packaged unit, stale preview rejection,
checksummed backup tuples, and owned integration removal/reapplication. A clearly
identified NVIDIA command fixture is used; no GPU is claimed by this gate.

A separate dpkg temporary-root test exercises unpack, remove, purge and reinstall
of the unchanged payload and checks preservation of private data. It does not
configure the package or bypass its GNOME 50 dependencies. These checks fail when
prerequisites are absent; they are unconditional parts of the existing gate.
The scripts require a disposable host and refuse existing installed backend paths.

See [repository controls](REPOSITORY-CONTROLS.md), [dependency updates](DEPENDABOT.md) and [releasing](RELEASING.md).
