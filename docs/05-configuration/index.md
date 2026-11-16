# 05 Configuration

| File | Contents |
|---|---|
| [reference.md](reference.md) | Every configuration key, type, default, and meaning |
| [example.yaml](example.yaml) | A complete, working example for a server inside a restricted network |
| [client-setup.md](client-setup.md) | How to point each tool at Larder, including offline-friendly settings |

## Rules

- One YAML file. `version: 1` is required.
- Unknown keys are errors, with the key path and a suggestion if a close key exists.
- Durations: `500ms`, `6s`, `5m`, `24h`. Sizes: `512MiB`, `20GiB`, `1TiB` (also bare bytes).
- Environment expansion: `${NAME}` and `${NAME:-default}` anywhere in string values.
- Reload with SIGHUP; see [concurrency and failure](../02-architecture/concurrency-and-failure.md) for what applies without restart.
- `larder config check` validates; `larder config defaults` prints everything with defaults.
