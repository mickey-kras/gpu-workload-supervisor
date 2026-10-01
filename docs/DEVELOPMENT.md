# Development

[Documentation](README.md) | [Repository](../README.md)

Use the Go version declared in [go.mod](../go.mod). From the repository root:

```sh
go test ./...
go vet ./...
```

The optional systemd integration suite requires a real user-systemd manager and host cgroup v2. It fails when those prerequisites are unavailable:

```sh
go test -race -count=1 -timeout=10m -tags=systemd_integration -run '^TestSystemd' -v ./internal/supervisor
```

It exercises lifecycle and ownership transitions, restart/restore, descendant release, draining and concurrent commands against real units and SQLite. Health endpoints are fixtures; failures and interrupted journal phases are injected. These tests do not qualify an operator's GPU, driver, workload API or boot automation. Complete [host qualification](DEPLOYMENT.md#qualify-the-host) before enabling execution.

See [repository controls](REPOSITORY-CONTROLS.md), [dependency updates](DEPENDABOT.md) and [releasing](RELEASING.md).
