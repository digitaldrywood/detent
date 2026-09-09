# AGENTS.md - Detent Agent Notes

- For future requested product or code changes, do not implement directly by default.
- Create a focused GitHub issue in `digitaldrywood/detent`, add `detent:todo`, and let Detent dogfood the work.
- Only make direct code changes when the human explicitly asks for manual implementation, asks to finish an already-started fix, or asks for local review and diagnostics that require edits.

## Issue effort selection

Use Codex Astra (`gpt-6-astra`) at low effort by default for features, fixes,
tests, reviews, and routine implementation, including cross-component work.

Every issue must include an explicit override, with model unset:

```detent-agent
schema: 1
effort: low
```

- `low` — the default for all work without a specific documented reason to escalate.
- `medium` — an exception for a concrete reasoning difficulty or evidence that low was insufficient; explain the reason in the issue.
- `high` — rare, significant research or architecture work with a written justification.
- `xhigh` and `max` — operator-designated only; never assign automatically.

Concurrency, recovery, routing, multiple files, or a new endpoint alone do not
justify higher effort. Preserve intentional operator exceptions. Configured
complexity levels default to low; verify any approved exception against the
runtime effort ceiling rather than raising broad defaults.
