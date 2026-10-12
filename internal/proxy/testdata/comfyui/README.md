# ComfyUI lifecycle fixture provenance

These are sanitized **source-derived** lifecycle fixtures, not captures from a
qualified GPU deployment. Reference: Comfy-Org/ComfyUI commit
`a7169322485d0049380fb207fa17e9fb3ec40486` (2026-09-29).

- `server.py`, blob `de0076a761db98a683c1ab1746f9c5914d1a8b52`:
  `POST /prompt` validates caller `prompt_id` as canonical UUID, enqueues
  `(number, prompt_id, prompt, extra_data, outputs_to_execute, sensitive)`;
  `GET /queue` strips the sensitive sixth element; `GET /history/{prompt_id}`
  returns the prompt-keyed history map.
- `execution.py`, blob `2e4cf646f89fbc5703c3e114f86e02b31629c570`:
  `PromptQueue.task_done` removes running work and stores prompt, outputs and
  `ExecutionStatus` under the prompt ID.
- `main.py`, blob `af82e3103b05b686b902defa87aa09073ecef22c`:
  `prompt_worker` calls `task_done` after executor returns, with
  `status_str='success' if e.success else 'error'`, `completed=e.success`.

The fixture replaces graphs with a synthetic `FixtureOutput` node, IDs with
stable synthetic UUIDs, and removes outputs/client data. The supervisor adds
the `gpu_workload_supervisor.attemptId` metadata. Tests replay this shape and
synthetically mutate correlation, status and queue observations. No custom node,
graph safety, CUDA release or deployed version is qualified by these tests.

Only correlated `success` with `completed: true` and subsequent absence from
both well-formed queue lists completes work. Error/cancelled histories remain
unresolved in this initial adapter, even though this source writes them after
executor return. The adapter does not interpret arbitrary error event payloads.
Before `task_done`, `/history/{prompt_id}` returns an empty map. A matching
queued/running tuple with the same prompt UUID and attemptId makes this an
ordinary pending poll, without completion or an error report. Empty history
without that queue correlation remains ambiguous and produces a diagnostic.
Old ComfyUI versions that ignore caller `prompt_id` are unsupported; missing
correlated history stays unresolved and must never cause automatic enqueue retry.
