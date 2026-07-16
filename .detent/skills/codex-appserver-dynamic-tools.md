---
name: codex-appserver-dynamic-tools
description: Add application-owned dynamic tools to Detent's Codex app-server backend without weakening worker lifecycle or sandbox guarantees.
when_to_use: Use when Detent needs Codex to call typed in-process tools during a turn, especially for live queries or confirmation-gated operations.
---

# Codex app-server dynamic tools

- Generate the installed Codex protocol schemas into Detent's temporary directory and inspect `ThreadStartParams`, `ThreadResumeParams`, and `ServerRequest` before changing the client; app-server fields are version-sensitive.
- Add dynamic tools through an optional runner backend interface so existing worker backends and ordinary turns remain unchanged.
- Register function specs in `dynamicTools` on `thread/start`. Do not send that field to `thread/resume`, whose schema does not accept it; reapply developer instructions, approval policy, sandbox, workspace, and model overrides supported by resume.
- Treat `item/tool/call` as a JSON-RPC server request in both response-wait and turn-stream loops. Decode `tool` and `arguments`, call the application handler, and respond on the same request ID with `contentItems` containing `inputText` plus an explicit `success` boolean.
- Always answer tool requests. Return an unsuccessful tool result for a missing handler or tool error so app-server cannot hang waiting for a response.
- For operator-facing tools, force approvals to `never`, use a read-only thread sandbox, omit writable turn policy, and instruct Codex to use only the application tools. Keep mutations proposal-only until the application records explicit confirmation.
- Reuse the existing Codex transport, process-group identity updates, workspace scratch preparation, cleanup, and thread resume path; do not introduce a second launcher or unmanaged temporary directory.
- Test fresh-thread tool registration, resumed developer instructions, tool-call request/response payloads, failure responses, sandbox restrictions, thread continuity, and scratch cleanup.
