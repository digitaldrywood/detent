# Comparing Detent With Adjacent Agent Tools

We build Detent as a self-hosted, board-native agent orchestrator for software delivery; this is how we stack it against nearby tools.

## Feature Matrix

| Capability | Detent | OpenAI Symphony | Copilot agent | Cursor | Hermes | OpenClaw |
|---|---|---|---|---|---|---|
| Self-hosted, no vendor control plane | ✅ | ✅ | ❌ | ❌ | ✅ | ✅ |
| Runs fully local / air-gappable | ✅ | ✅ | ❌ | ❌ | ✅ | ✅ |
| Board/tracker-native (issue→PR) | ✅ GH Projects, issue fields, or labels | ✅ Linear | ✅ GH Issues | ❌ | ❌ | ❌ |
| Deterministic gated merge train | ✅ | 🟡 per spec | ❌ | ❌ | ❌ | ❌ |
| Budget / cost caps | ✅ | ❌ | 🟡 | 🟡 | ❌ | ❌ |
| Multi-project | ✅ | ❌ | ✅ | ✅ | ❌ | ❌ |
| Multi-instance fleet governance | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| Model-agnostic, BYO incl. local | 🟡 codex now, seam shipped | ❌ Codex | ✅ vendor-managed | ✅ vendor-managed | ✅ | ✅ |
| Local skills / workflows (your e2e etc.) | ✅ | 🟡 | ❌ | 🟡 | ✅ | ✅ |
| Open source | ✅ MIT | ✅ Apache-2.0 | ❌ | ❌ | ✅ MIT | ✅ MIT |
| Free (BYO model cost) | ✅ | ✅ | ❌ paid | ❌ paid | ✅ | ✅ |
| Single static binary | ✅ | ❌ Elixir/BEAM | — SaaS | — SaaS | ❌ gateway | ❌ gateway |
| ~5-min setup | ✅ | ❌ | ✅ zero-install | ✅ | 🟡 | 🟡 |

## What Each One Is

- **Detent**: [digitaldrywood/detent](https://github.com/digitaldrywood/detent) is our single-binary Go orchestrator for GitHub-native issue-to-PR work using ProjectV2 or boardless status sources.
- **OpenAI Symphony**: [openai/symphony](https://github.com/openai/symphony) is our origin point: an Apache-2.0 spec plus Elixir reference implementation for Codex on Linear.
- **GitHub Copilot coding agent**: [GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent) is GitHub's paid issue/prompt-to-branch-and-PR agent.
- **Cursor**: [Cursor cloud agents](https://cursor.com/cloud) is an IDE-first agent product with cloud/background agents, automations, and optional self-hosted workers.
- **Hermes**: [NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent) is Nous's MIT personal assistant with memory, skills, model providers, and messaging gateway.
- **OpenClaw**: [openclaw/openclaw](https://github.com/openclaw/openclaw) is an MIT personal assistant centered on a local gateway and cross-channel automation.

## Where We're Different

We own the orchestration loop: no Detent vendor control plane, GitHub-native status sources, Detent's own Kanban board, deterministic gates, and a serialized merge train. We also care about operating fleets, not just launching one agent: multi-instance ownership, budget checks, local skills, and a single static binary with a setup path we expect to be measured in minutes. Copilot and Cursor have closed much of the "runs near my code" gap and win zero-install inside their platforms, but they do not give us the same board-native release runtime under our control.

_Last updated: June 19, 2026; verify vendor pricing/models before relying on them._
