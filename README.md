# CLIProxyAPI plugins

Third-party plugins for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).
This repository is a plugin store source you add to the host configuration.

## What this is

Each subdirectory is one plugin. The host loads a C-ABI library
(`.dylib` / `.so` / `.dll`) and talks to it over the plugin RPC. Plugins
version independently; `registry.json` is the store catalog.

| ID | What it does | Docs |
| --- | --- | --- |
| `auth-cursor` | Expose Cursor models through CLIProxyAPI | [auth-cursor/README.md](auth-cursor/README.md) |

Plugin IDs (`auth-cursor`) are not provider keys (`cursor`). Credentials,
`owned_by` on `/v1/models`, and host blocks such as `oauth-excluded-models`
use the provider key. Details live in the plugin README.

## Use

1. CLIProxyAPI built with CGO (`X-CPA-SUPPORT-PLUGIN: 1` on management responses).
2. A management key (`remote-management.secret-key`).
3. Add this store and enable plugins. Under Homebrew / launchd / systemd,
   `plugins.dir` must be an **absolute writable path** — a relative `"plugins"`
   or `~` is resolved against `/`.

```yaml
plugins:
  enabled: true
  dir: "/Users/<you>/.cli-proxy-api/plugins"
  store-sources:
    - "https://raw.githubusercontent.com/UNICKCHENG/cliproxyapi-plugins/main/registry.json"
```

4. Restart the host, then install from the store:

```bash
curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  "localhost:8317/v0/management/plugin-store/auth-cursor/install"
```

Credentials, models, proxying, and troubleshooting:
[auth-cursor/README.md](auth-cursor/README.md).

## Local development

```bash
cd auth-cursor
make test
make build          # dist/auth-cursor.<ext>
```

Copy the library into `plugins/<GOOS>/<GOARCH>/` and restart the host.
If the plugin was previously installed from the store, the host loads a
**versioned** filename (`auth-cursor-v2.0.0.dylib`). Copying only
`auth-cursor.dylib` is not enough — see
[auth-cursor/README.md — Development](auth-cursor/README.md#development).

Releases: tag `<plugin>-v<version>` (for example `auth-cursor-v2.0.0`).
The workflow publishes platform archives and updates `registry.json`.

## License

[MIT](LICENSE)
