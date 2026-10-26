# Router configuration spike (#396)

Source: [llama.cpp v0.6.0](https://github.com/ggml-org/llama.cpp/tree/d81235049384534c167caea52b85a694f6103d14),
commit `d81235049384534c167caea52b85a694f6103d14`.
The unit and preset are source-derived experiment inputs, not installation
templates. These tests do not qualify a router binary or GPU lifecycle.
The existing model-response fixtures are synthetic protocol replays.

`router.service` selects explicit loading, one model, disabled idle sleep,
loopback binding and a private API-key file. `models.ini` also disables startup
loading: `--no-models-autoload` alone does not do that. The absolute paths are
placeholders. The commit in the executable path does not check binary identity.
An experiment must independently check the build, use a nonempty protected key
file, trusted preset and model files and an isolated environment and cache. Do not
install this unit or point a production profile at it.

Source findings:

- [server-models.cpp](https://github.com/ggml-org/llama.cpp/blob/d81235049384534c167caea52b85a694f6103d14/tools/server/server-models.cpp):
  `is_autoload` permits query overrides, including `true` and `1`.
  `load_startup_models` honors preset startup loading separately. Model-specific
  GET requests, including metrics, can autoload. A nonempty `reload` query on
  the model-list handler reloads the registry.
- [server.cpp](https://github.com/ggml-org/llama.cpp/blob/d81235049384534c167caea52b85a694f6103d14/tools/server/server.cpp):
  `/models` and `/v1/models` share that handler. Management includes POST
  `/models`, `/models/load`, `/models/unload`, DELETE `/models` and GET
  `/models/sse`.
- [server-http.cpp](https://github.com/ggml-org/llama.cpp/blob/d81235049384534c167caea52b85a694f6103d14/tools/server/server-http.cpp):
  The router skips API-key validation if the configuration has no keys. Loopback alone does
  not isolate the management API from other local clients. The router removes child API keys. Child containment still needs qualification.

`router_spike_test.go` exercises the existing launch and HTTP readiness paths.
Router launches remain rejected. A healthy endpoint is insufficient when the selected model is not ready. Examples are an unloaded model, a model in the loading state, and a model that failed or exited. Readiness sends only
query-free `/health` and `/v1/models` requests. Proxy regressions deny management
and metrics route configuration and per-request autoload before admission.

Remaining gates: the direct probe accepts missing status for direct-server
compatibility and rejects any extra catalog entry, even unloaded. It is not a
router readiness contract. Before activation, an authenticated router adapter must meet these requirements:

- Get explicit selected-model status.
- Handle the full registry.
- Bind presets and build identity.
- Show parity for stream completion and drain, child cgroup and GPU release, and capacity.

The native readiness client does not use API authentication. Keep direct and adopted launches and all optional
infrastructure unchanged until those gates pass.

Run the replay checks with:

```sh
go test -race ./internal/runtime ./internal/proxy -run RouterSpike
```
