# Supported application discovery implementation plan

> **For agentic workers:** Use superpowers:executing-plans. Steps use checkbox syntax.

**Goal:** Read-only four-app discovery with native inventory and explicit file/folder fallback.
**Architecture:** Separate observational candidates from executable workload profiles. Bound and cancel requests; reuse the direct HTTP transport. Keep saved and interrupted configuration unchanged.
**Tech Stack:** Go standard library, existing direct HTTP transport and systemctl reader.
**Spec:** GitHub issue #211.

## Global constraints
- Only ComfyUI, Ollama, llama.cpp and vLLM.
- No disk scanning, application execution, GPU mutation, downloads or writes.
- Endpoint/file candidates never establish lifecycle control or model compatibility.

## Review focus
- Unknown/malformed inventories must not become successful empty inventories.
- Redirects, proxy variables and custom URLs must not reach non-loopback hosts.
- llama.cpp reload and routed autoload endpoints must never be used.
- Saved profiles survive unreachable instances; aliases are not distinct models.
- Unit labels do not establish application identity or lifecycle control.

### Task 1: Native candidate probes
Files: internal/setup/application_probe.go, application_models.go, application_probe_test.go.
Interface: Probe(ctx, ProbeRequest) (ApplicationCandidate,error); DecodeProbe(io.Reader).
- [x] Write fixture tests for all four apps, malformed/unsupported responses, cloud models, aliases, cancellation, explicit references and redirects.
- [x] Observe failing tests before implementation.
- [x] Implement read-only bounded GET adapters and metadata-only reference validation.
- [x] Run focused tests.

### Task 2: Discovery and CLI
Files: internal/setup/discovery.go, application_discovery.go, application_discovery_test.go; cmd/gpu-setup/main.go and probe_test.go.
- [x] Test unrelated services omitted and recognized service metadata never marks lifecycle verified.
- [x] Implement supported candidates and CLI probe while preserving saved/pending request.
- [x] Document CLI contract and run full Go suite.
