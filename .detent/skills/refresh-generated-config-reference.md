---
name: refresh-generated-config-reference
description: Keep Detent generated configuration references current after changing YAML-visible config fields, defaults, or validation.
when_to_use: Use when a Detent config change passes local tests but fresh CI reports stale docs/config.md or config.reference.yaml.
---

# Refresh generated config references reliably

- Fetch current `origin/main` before diagnosing drift; the configdoc generator may have landed after the feature branch was created.
- Rebase a clean issue branch when CI tests a newer base that contains configdoc code absent locally.
- Run `make generate` and review the resulting `docs/config.md` and `config.reference.yaml` diff. Do not hand-edit generated reference text.
- Clear only Go test-result caching with `go clean -testcache` when a prior local gate passed unexpectedly. The configdoc test inspects repository config sources that may not invalidate Go's package cache.
- Run `go test ./internal/config/configdoc`, then rerun `make check` from the top.
- Push the refreshed head and watch CI for that exact commit SHA; ignore failures attached only to the obsolete pre-rebase head.
