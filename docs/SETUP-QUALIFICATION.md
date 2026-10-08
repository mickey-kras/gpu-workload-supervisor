# Setup candidate qualification

CI tests protocol, widget fixtures, real systemd lifecycle and packaged payloads.
They do not establish GNOME accessibility, runtime/model compatibility or GPU
release on a deployment host. Keep #332, #333 and #335 acceptance open until
observed results pass; record host details privately.

## Pin the candidate

Use the successful PR validation run's `candidate-<built-commit>-<run>-<attempt>`
artifact. Retain its artifact ID/digest, run URL, `candidate-manifest.json` and
`checksums.txt`. The manifest distinguishes the tested merge commit from the PR
head and base. Verify every payload before installing:

```sh
sha256sum --check checksums.txt
```

Test exactly that payload; a later build or changed PR head needs qualification
again. This is a snapshot candidate, not a published release. Current activation
policy does not permit a snapshot to upgrade an activated stable release. Use
an isolated compatible desktop account/state for snapshot qualification; preserve
the existing deployment and its complete rollback tuple.

## Observed checks

Before testing, finish active work, use existing controls to reach healthy Idle
with admission closed, and pause external application automation. Retain external
unit, drop-in and launch-file hashes plus the current configuration/state backup.

| Check | Required observation |
| --- | --- |
| Application choice | Four application cards precede models. One installation per app is selected without entering technical fields. Arbitrarily named supported services are detected from executable evidence. |
| Models | Relevant models appear beneath their application. ComfyUI has no chooser. Selected models become distinct named controls; duplicate application/model bindings are rejected. Unsupported model changes have actionable guidance. |
| Navigation | Back, cancellation, refresh, retry, saved choices, defer/reopen and editing retain deliberate changes and invalidate stale review. One primary action is visible on each screen. |
| Native usability | Keyboard-only and screen-reader journeys cover cards, model choices, labeled gears, errors, Back and Finish. Verify focus order, small windows and supported display scaling without clipped controls or footer overlap. |
| Metadata-only setup | Recognized stopped applications remain stopped while choosing and reviewing. Confirm service state/invocation and no model load or GPU activity caused by discovery. |
| Existing launches | Supported llama.cpp options, model pre-start checks and drop-ins survive adoption byte-for-byte. Start uses the preserved effective configuration; changes after preview are refused. |
| Prior compatible deployment | Opening setup and reading current status leave the database, WAL, schema, profile, deployment marker and external service state unchanged. Temporary actions remain unavailable on an older schema until normal backed-up activation installs the current schema; verify that the retained backup contains the compatible old schema. |
| Temporary inventory | Where needed and eligible, explicit consent precedes startup. Completion and cancellation restore the stopped service, verify GPU/cgroup release and retain closed admission. A previously running service is never stopped by detection. |
| Interrupted cleanup | Cleanup failures are visible and block ordinary controls. Reopening shows the persisted session. Explicit cleanup succeeds only for its recorded invocation and control generation; changed ownership or restarted services are refused. |
| Activation and switching | Finish confirms the summary after work is quiescent. Verify model identity, readiness, exclusive switching and GPU release across the configured runtimes, including multiple Ollama models. |
| Preservation | Removing a configured workload removes supervisor configuration only. Applications, models, workflows, external units/drop-ins and launch options remain unchanged. Existing ComfyUI closure on switching remains unchanged. |
| Session lifecycle | Verify extension discovery, reopen, logout/login reconciliation and package behavior on the supported GNOME session. |

Record candidate identity, each observed result and any failure privately. Do not
merge or publish based solely on automated results or an unobserved checklist.
