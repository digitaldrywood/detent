# Comparing Detent With Adjacent Agent Tools

We build Detent as a board-native agent orchestrator for software delivery, available as Detent Cloud or as a self-hosted Hub; this is how we stack it against nearby tools.

## Feature Matrix

| Capability | Detent | Copilot agent | Cursor | Hermes | OpenClaw | Hyperagent |
|---|---|---|---|---|---|---|
| Self-hosted, no vendor control plane | ✅ self-hosted Hub | ❌ | ❌ | ✅ | ✅ | ❌ hosted |
| Agents run on your own machines | ✅ enrolled runners | ❌ | 🟡 self-hosted workers | ✅ | ✅ | ❌ cloud |
| Board/tracker-native (issue→change) | ✅ native tracker, optional GitHub | ✅ GH Issues | ❌ | ❌ | ❌ | ❌ own workspace |
| Deterministic gated landing | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| Budget / cost caps | ✅ | 🟡 | 🟡 | ❌ | ❌ | ✅ hosted controls |
| Multi-project | ✅ | ✅ | ✅ | ❌ | ❌ | 🟡 workspace-scoped |
| Runner fleet routing and capacity | ✅ | ❌ | ❌ | ❌ | ❌ | 🟡 hosted agent controls |
| Model choice, BYO subscription | ✅ Codex and Claude Code | ✅ vendor-managed | ✅ vendor-managed | ✅ | ✅ | ❌ cloud-managed |
| Local skills / workflows (your e2e etc.) | ✅ | ❌ | 🟡 | ✅ | ✅ | ✅ hosted skills/knowledge |
| Multi-channel triggers | 🟡 tracker, conversations, MCP | 🟡 GitHub | 🟡 IDE/cloud tasks | ✅ messaging gateway | ✅ local gateway | ✅ Slack, schedules, webhooks, email, Telegram, Live Mode |
| Source available | ✅ FSL-1.1-ALv2 | ❌ | ❌ | ✅ MIT | ✅ MIT | ❌ closed-source |
| Single static binary | ✅ | — SaaS | — SaaS | ❌ gateway | ❌ gateway | — SaaS |

## What Each One Is

- **Detent**: [digitaldrywood/detent](https://github.com/digitaldrywood/detent) is our single-binary Go orchestrator: a Hub (Detent Cloud or self-hosted) holds the board and policy, and enrolled runners execute issues as gated changes.
- **GitHub Copilot coding agent**: [GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent) is GitHub's paid issue/prompt-to-branch-and-PR agent.
- **Cursor**: [Cursor cloud agents](https://cursor.com/cloud) is an IDE-first agent product with cloud/background agents, automations, and optional self-hosted workers.
- **Hermes**: [NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent) is Nous's MIT personal assistant with memory, skills, model providers, and messaging gateway.
- **OpenClaw**: [openclaw/openclaw](https://github.com/openclaw/openclaw) is an MIT personal assistant centered on a local gateway and cross-channel automation.
- **Hyperagent**: [Hyperagent](https://www.hyperagent.com/) is Airtable's closed-source, cloud-hosted agent OS / enterprise teammate platform ([confirmed by Browserbase](https://www.browserbase.com/blog/case-study-hyperagent)) for persistent agents with identity, tools, skills, knowledge, model/budget controls, multi-channel triggers, and a first-class [Library](https://www.hyperagent.com/docs/concepts/library) for artifacts.

## Where We're Different

We own the orchestration loop: a native board, deterministic gates, and serialized landing, with the Hub either hosted by us or run by you. Agents run on runners you enroll, under your own Codex or Claude Code sign-in, next to your code and tools. We also build for operating fleets of agents, with runner routing and capacity, budget checks, and local skills. Copilot and Cursor have closed much of the "runs near my code" gap and win zero-install inside their platforms, but they do not give us the same board-native release runtime under our control.

_Last updated: October 7, 2026; verify vendor pricing/models before relying on them._
