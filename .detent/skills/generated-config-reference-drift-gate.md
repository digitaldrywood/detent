---
name: generated-config-reference-drift-gate
description: Build exhaustive generated configuration docs from typed config, effective defaults, and real validators, then keep them current with a non-mutating check.
when_to_use: Use when a Go project's YAML configuration surface is too large for a hand-maintained reference and new fields, defaults, or validation rules must fail CI if documentation drifts.
---

# Generated config reference drift gate

Reflect from the actual root config type and recurse only through types that are
part of the operator-facing schema. Treat dynamic mappings as explicit unions
of their supported typed variants. Keep paths canonical with `[]` for list
elements, and test that every YAML tag in the source package appears in the
generated path set.

Read defaults through the same constructor and normalization path used by the
loader. Render environment-dependent defaults symbolically so output is stable
across operating systems and temporary directories. For conditionally enabled
subsystems and list elements, initialize the parent, normalize it, and label
the resulting value as conditional.

Derive validation text by sending boundary probes through the real loader and
validator. Add focused scenario probes for cross-field alternatives that
single-field mutation cannot reach, such as authentication credential sets or
backend-specific option unions. Preserve validator messages instead of
maintaining a second hand-written rule catalog.

Keep hand-written prose outside generated markers. Make normal generation
rewrite only the marked block and the fully generated reference file. Provide
a check-only mode that renders in memory, compares exact bytes, and reports the
command that refreshes stale artifacts.

Run the check-only target before any build prerequisite that regenerates files.
Cover source-tag completeness, important capability discovery, validator rule
surfacing, conditional requiredness, current generated artifacts, and
human-authored samples parsed through the real loader.
