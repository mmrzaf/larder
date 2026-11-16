# serve and configuration commands

## `larder serve`

Runs the server: public listener, admin listener, workers. Startup steps: parse and validate configuration, take the lock, run recovery ([storage layout](../02-architecture/storage-layout.md)), open the index, start listeners, start workers, log `listening` with both addresses.

Flags: `--listen ADDR` and `--admin-listen ADDR` override configuration; `--offline` starts in offline mode.

Signals: SIGTERM and SIGINT shut down gracefully; SIGHUP reloads configuration.

## `larder config check`

Loads the configuration exactly as `serve` would, validates every field, and reports all errors at once (not only the first). Checks include unknown keys, duplicate repo names or mounts, overlapping mounts, invalid regular expressions, unparsable URLs and sizes, unknown kinds, watermark ordering, and permissions of an existing `data_dir`. Exit 2 on error. `--connect` additionally sends a HEAD request through each upstream and prints reachability and latency (the only configuration check that touches the network).

## `larder config print`

Prints the effective configuration after defaults and environment expansion, with secrets (headers, proxy credentials) replaced by `***`.

## `larder config defaults`

Prints a complete annotated configuration with every default, suitable as a starting point.

## `larder healthcheck`

Requests `--url` (default `http://<admin listen>/readyz`) and exits 0 when the answer is 200, else 1. Intended for container health checks in images without a shell.

## `larder version`

Prints version, commit, build date, Go version. `--json` supported.

## Environment

| Variable | Effect |
|---|---|
| `LARDER_CONFIG` | configuration path |
| `LARDER_DATA_DIR` | overrides `data_dir` |
| `LARDER_LISTEN` | overrides `listen` |
| `LARDER_ADMIN_LISTEN` | overrides `admin.listen` |
| `LARDER_LOG_LEVEL` | overrides `log.level` |

`${NAME}` and `${NAME:-default}` references inside the YAML are expanded from the environment when the file is loaded.
