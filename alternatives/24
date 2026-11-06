# Contributing

Detent is a Go agent orchestrator delivered as a single binary. Keep changes small, scoped to one issue, and aligned with the existing conventions.

The rules for working in this repository live in two files: [AGENTS.md](AGENTS.md) for coding, testing and authoring rules (it applies to people and agents alike) and [WORKFLOW.md](WORKFLOW.md) for branching, validation, review and release. This page only covers getting started.

## Contributor License Agreement (CLA)

The CLA bot asks first-time contributors to read and sign the [individual
Contributor License Agreement](CLA.md) on their first pull request. Sign by
replying to that pull request with this exact phrase:

> I have read the CLA Document and I hereby sign the CLA

The bot records your signature, so you do not need to sign again on later pull
requests.

## Setup

You need Go 1.26, Git, and Node 24 with npm.

```sh
git clone https://github.com/digitaldrywood/detent.git
cd detent
make setup
make app
make dev
```

`make setup` installs Air, Templ, sqlc, goose, golangci-lint v2 and npm dependencies. `make app` builds the embedded conversation app. `make dev` runs Air with `ENV=dev` and `LOG_LEVEL=debug`, builds `./tmp/detent`, and streams build and application output to `tmp/air-combined.log`.

Do not start a second server on `127.0.0.1:4000` when another Detent process uses it; run `./tmp/detent --port 0` for an ephemeral port. Logging and `global.yaml` discovery are described in the [CLI reference](docs/cli.md) and [configuration reference](docs/config.md).

## Validation

While working, run focused tests for the packages you change. `make check-landing` is the landing gate and `make check` the optional full local diagnostic; [WORKFLOW.md](WORKFLOW.md#validation) lists what each runs. Run `make generate` after changing templates, SQL queries, migrations or Tailwind inputs.

## UI Surface Guardrails

High-impact UI changes require explicit issue authorization before implementation starts. If an issue does not describe the affected surface and real-estate tradeoff, choose a local or contextual treatment instead of a persistent global surface, or stop and ask for a more specific issue.

A high-impact UI change adds or significantly alters any of the following:

- Persistent banners, toasts, alerts, modals, drawers, or other elevated surfaces.
- First-viewport real estate on primary operator screens such as Board, Fleet, Reports, Analytics, Health, API Keys, or Settings.
- Kanban lane layout, card density, column spacing, sticky headers, or top-of-board summary regions.
- Navigation, status counters, page chrome, or live dashboard regions.
- Responsive behavior that changes what is visible on desktop, laptop, or mobile viewports.

Issues that authorize high-impact UI work must include a UI surface contract covering the affected screen and component area, current and desired behavior, whether persistent global messaging is allowed, what information must stay contextual on cards, rows, or details, density expectations when relevant, an expected first-viewport screenshot, mockup, or text description, and the screenshot or browser verification expected.

Persistent top-of-screen messages require explicit issue authorization. Do not infer permission for global banners, alerts, or notification rows from generic words like "alert", "blocked", "status", or "notify". Workflow state that is already visible in a Kanban card defaults to contextual card indicators instead of duplicated global messaging.

Small copy changes, icon swaps, and local component styling do not need a heavyweight UI surface contract unless they affect layout, persistent surfaces, density, or viewport visibility. Visual changes to primary operator screens require screenshot or browser verification before review.

## Changes and commits

Start from current `origin/develop` and use conventional commits (`feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`, `perf`), for example `fix(store): close rows after migration lookup`. Work is tracked in the native Detent Cloud project and lands on `develop` through Detent's native landing, as [WORKFLOW.md](WORKFLOW.md#branching-and-landing) describes; pull requests are used only for release-branch picks. Do not bypass hooks.
