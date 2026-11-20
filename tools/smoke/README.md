# Local smoke suite

From this directory, run `bin/smoke all --profile both --ref origin/develop` or select one or more names from `bin/smoke --list`. `--ref` also accepts a clean Detent worktree. The launcher builds that commit once, starts an isolated Hub, PostgreSQL, runner and Git mirror for each replay, then stops those processes after collecting evidence.

Use Python 3.10+, the project's Go and Node versions, PostgreSQL 17, Git, OpenSSL and Ruby. If `pg_config` points to a different PostgreSQL installation, set `DETENT_SMOKE_POSTGRES` to the PostgreSQL 17 binary directory. The runner copies only the existing Codex credential file into its private home. Set `DETENT_SMOKE_CODEX_AUTH` to choose that file explicitly. The launcher never prompts for credentials.

The fixture defaults to the todo canary Git repository. Set `DETENT_SMOKE_FIXTURE` and optionally `DETENT_SMOKE_FIXTURE_REF` to select a local checkout or a fixture branch with `smoke/scenarios` and `smoke/profiles`. The source under test starts at `canary-baseline-v2`. Each scenario directory contains `scenario.json`, `issue.json` and `expected.json`. Existing scenarios link to the corresponding todo fixtures. A scenario can set policy values, apply a source patch or name a prerequisite and its staged-file operator check. The backlog scenario lists its projects and holding lanes in the definition.

The baseline profile uses tagged todo policy. The pyro profile uses the same todo source and command with the acceptance, promotion and security-audit configuration declared by the fixture. The review rejection scenario explicitly enables implementation review to exercise that decision in both profiles.

Each replay starts from fresh private state. Loopback ports are pinned for a replay and reused by subsequent scenarios after shutdown. Set `--parallel` or `--port` to fit the available local capacity. The table reports the first difference per scenario and the path to its run log, Hub and runner logs, and receipt snapshot. Set `DETENT_SMOKE_ARTIFACTS` to keep these artifacts outside the default temporary directory. Keep the artifact directory private because it also contains the isolated service state.

A trusted optional private pack can set `DETENT_SMOKE_PRIVATE_PACK` to a directory containing `scenarios/<name>/run`. The launcher passes the pinned commit, profile and private artifact directory to that executable and uses its exit status and first `FAIL` line. Keep private scenario code and inputs in that pack.
