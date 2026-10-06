# Local desktop controls

[Documentation](README.md) | [Repository](../README.md)

## Native local controls

`clients/gnome/gpu-workload-supervisor@local` implements the GNOME Shell 50
extension shipped with the local operator. It uses native SystemIndicator and
QuickMenuToggle controls and invokes only `/usr/bin/gpu-operator`, with one bounded
JSON request per process. No proxy, gateway, Job Broker, listener or persistent UI
service is required. The local OS account supplies operator authority; ownership
confirmation expresses intent, not verified human presence.

The committed toggle is OFF under Supervisor ownership and ON under User ownership.
Take Control confirms preserving the verified current workload. Stop and Return
confirms stopping work and returning Supervisor ownership in Idle. User workload
selection is immediate; selecting the active workload does nothing. Normal menus
show configured labels. Details retains owner, requested and active workloads,
phase, health, admission and conservative observation freshness.

The client never optimistically changes committed ownership. Pending calls, stale
observations, errors and recovery requirements disable mutations. One call may be
outstanding; status polling backs off from five to sixty seconds and never overlaps
a mutation. Freshness expires thirty seconds after dispatch, using monotonic time.
Versions remain decimal strings. Retired enable generations, stale decisions and
lower same-incarnation versions are rejected. Disconnects require fresh status;
mutations are never replayed. Client read deadlines are 75 seconds for status and
33 minutes for mutations. These exceed the backend's maximum operation plus
cleanup/finalization and input/output budgets (74 seconds and 1,934 seconds,
respectively); a client deadline leaves the outcome uncertain. Disable cancels local reads and removes UI resources,
without killing the backend. The backend must independently preserve admitted
operations across Shell restart. There is no recovery control in the extension.

Run contract, model, and mocked lifecycle tests with `npm test --prefix clients/gnome`.
Run the native bounded-stream smoke test with
`gjs -m clients/gnome/tests/transport.gjs.js`. The latter needs GJS and is not a
Shell qualification. Before enablement, qualify a real GNOME 50 session: keyboard
and visual access to the toggle/menu/Details, cancel and confirm both ownership
dialogs, third-workload selection, active no-op, pending repeated clicks, stale and
faulted observations, disable/re-enable and Shell restart during admitted work.
Confirm backend survival and later fresh status with the actual deployment.
Headless tests do not provide this evidence. Packaging alone does not enable the
extension or qualify a distribution.

Browser automation is not implemented. There is no dashboard or browser client,
and no transport grants browser callers the local operator's authority.
