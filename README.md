# symphony

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Symphony is an agent orchestrator for tracker-backed work queues, delivered as a single Go binary with pluggable connectors, isolated worktrees, and live web and terminal dashboards.

The previous Elixir implementation is retained only as a cutover reference and should remain archived after the Go repository is renamed to `digitaldrywood/symphony`.

## Development

Run the local hot-reload loop with:

```bash
make dev
```

The target runs Air with `SYMPHONY_ENV=dev` and `SYMPHONY_LOG_LEVEL=debug`, and writes the combined stream to `tmp/air-combined.log`.

## Logging

Symphony logs with `log/slog`.

- `SYMPHONY_ENV=dev`, `development`, or `local` enables tint text logs.
- `SYMPHONY_ENV=prod` or any other non-development value keeps JSON logs.
- When `SYMPHONY_ENV` is unset, interactive stdout TTY runs use tint text logs; non-TTY runs use JSON logs.
- `SYMPHONY_LOG_LEVEL` accepts `debug`, `info`, `warn`, `warning`, and `error`.
- Text logs are written to stdout; JSON logs are written to stderr.

## License

Symphony is released under the [MIT License](LICENSE).
