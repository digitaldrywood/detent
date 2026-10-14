---
name: remote-memory-cli-probe
description: Validate a newly built diagnostic CLI on a Linux host without installing it or creating remote scratch files.
when_to_use: A remote Linux instance needs a read-only diagnostic from unmerged code, its installed binary must remain untouched, and no managed remote temporary directory is available.
---

# Standalone diagnostic through a Linux memory file

1. Confirm the command is a standalone read-only probe. Do not use this method
   for service startup, deployment, migrations, write probes, or process control.
2. Read the remote architecture and verify Python exposes `os.memfd_create` over
   an existing authorized SSH connection. This is Linux-specific; do not assume
   another operating system supports it.
3. Cross-build the CLI for that architecture into the worker's provided `TMPDIR`.
   For a pure-Go Linux binary, use `GOOS=linux`, the observed `GOARCH`, and
   `CGO_ENABLED=0`; preserve the normal content-addressed Go caches.
4. Stream that binary as SSH stdin to a short Python receiver. The receiver calls
   `os.memfd_create(name, 0)`, copies stdin into the descriptor in bounded chunks,
   flushes the writer, and executes `/proc/self/fd/<fd>` with an explicit argument
   array for the diagnostic. The descriptor must remain open across exec; this
   replaces only the newly created SSH child, never the service process.
5. Construct the remote Python command with `shlex.quote`, pass SSH arguments
   through a subprocess argument array, and capture stdout/stderr locally under
   `TMPDIR`. Do not put credentials in command arguments or published output.
6. Inspect the diagnostic's exit status and structured results. A doctor failure
   can represent expected findings; distinguish those from startup, transport,
   kernel execution-policy, or unreadable-evidence failures. Do not bypass a
   host policy that rejects executable memory files.

This leaves the installed executable and configuration untouched and creates no
persistent remote executable. The command itself still needs review: an ephemeral
binary can mutate state just as an installed binary can. Validate the actual
runtime config/database paths before interpreting missing-schema warnings.
