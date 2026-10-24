---
name: durable-machine-deferrals
description: Preserve machine-resolvable worker waits without charging terminal progress brakes or repeatedly launching full sessions.
when_to_use: Use when a worker correctly stops on tracked dependencies, remote capacity, or another machine-resolvable condition that must survive orchestrator restart.
---

# Durable machine deferrals

1. Reproduce the wait being misclassified as failure or no progress before changing the brake.
2. Accept only a structured wait signal with no human action. Validate every declared reference through the owning backend and require at least one unresolved real resource.
3. Log and reject malformed, fabricated, self-referential, or missing references. Preserve the original brake behavior for rejected signals.
4. Persist a distinct deferral reason and the validated resource identities in durable attempt metadata. Do not rely on an in-memory retry entry for restart safety.
5. Release the worker claim without scheduling another full session.
6. Before dispatch, inspect only the latest applicable attempt, batch-refresh its recorded resources, and suppress launch while any remain unresolved. Treat refresh failures as continued deferral and release the issue when every resource becomes terminal.
7. Test the terminal-brake boundary, human-action precedence, rejected references, restart reconstruction, unresolved suppression, and terminal release.
