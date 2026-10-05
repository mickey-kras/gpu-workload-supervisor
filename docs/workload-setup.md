# Manage workloads

Open **Manage workloads** from GPU Control in GNOME's top-right Quick Settings
menu. The packaged setup application is also available in the application list.

1. Choose ComfyUI, Ollama, llama.cpp or vLLM and select **Add workload**.
2. Choose a detected instance. **Refresh discovery** reads its existing inventory;
   it never starts an application or loads a model.
3. Choose a native model, or an explicit model file/folder. ComfyUI skips model
   selection because workflows choose models. File selection does not establish
   compatibility.
4. **Save drafts** to finish later. Drafts retain application, address/path and model choices, and Advanced launch binding fields. Saved bindings remain unverified; fingerprints are recomputed during verification. Drafts are separate from selectable workloads.
5. To configure an existing isolated service, open **Advanced launch binding**.
   Supply its service, cgroup and health URL. Native model bindings also require
   an instance ID, exact model ID, base URL and loaded service file. Setup reads
   the file fingerprint and verifies the binding without changing the service.
6. Select **Verify binding and add for review**, then **Review configuration**.
   Switch to Idle and finish active jobs before confirming **Apply configuration**.

Each model needs a distinct existing launch unit. Shared-unit model presets are
not supported. Setup does not create or rewrite launch services. Model identity
is checked again before GPU admission when switching workloads.

Configured workloads can be renamed or edited. **Remove from supervisor** changes
only supervisor configuration. Active/referenced workloads cannot be removed;
finish their work and switch to Idle first. Applications, models, workflows and
external configuration are preserved.

**Set up later** closes the window without applying configuration. Save drafts
first if you want to keep new choices. A stale-draft error requires reopening
Manage workloads before retrying. Interrupted activation resumes its recorded
configuration, with editing disabled until it completes.

Native GTK keyboard/accessibility checks and real GPU qualification remain part
of the GNOME/package host acceptance in issues #146 and #148. Automated widget
and protocol tests do not qualify the host installation.
