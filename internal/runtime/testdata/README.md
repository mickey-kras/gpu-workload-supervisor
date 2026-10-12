# Native API fixtures

These are sanitized, source-derived examples, not host captures. Model names,
aliases, digests, timestamps, sizes and arguments are replaced with inert values.
Only fields needed to exercise identity and readiness parsing are retained.

- `ollama-v0.40.3/api-ps-*.json`: `ProcessResponse` and
  `ProcessModelResponse` in [Ollama v0.40.3 api/types.go](https://github.com/ollama/ollama/blob/v0.40.3/api/types.go),
  for `GET /api/ps`.
- `llama.cpp-v0.6.0/models-*.json`: `get_router_models` in
  [llama.cpp v0.6.0 tools/server/server-models.cpp](https://github.com/ggml-org/llama.cpp/blob/v0.6.0/tools/server/server-models.cpp)
  and the status mapping in
  [server-models.h](https://github.com/ggml-org/llama.cpp/blob/v0.6.0/tools/server/server-models.h),
  for `GET /models`. Failed children report `unloaded` with `failed: true`
  and nonzero `exit_code`.

Test cases that replace fields or supply inline payloads are synthetic mutations,
including contradictory loaded/failure states and unsupported shapes. The
`/models` HTTP replay feeds the existing model-list parser directly; native
llama.cpp readiness still requests `/health` and `/v1/models`. This replay does
not qualify router binding, load/unload, admission or GPU release behavior.
