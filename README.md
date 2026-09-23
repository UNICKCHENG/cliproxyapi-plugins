# CLIProxyAPI plugins

Third-party plugins for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Add this repository as a store source; the host installs plugins from `registry.json`.

Each subdirectory is one plugin and versions on its own.

| ID | Purpose | Docs |
| --- | --- | --- |
| `auth-cursor` | Call Cursor models through CLIProxyAPI | [auth-cursor/README.md](auth-cursor/README.md) |
| `auth-commandcode` | Call Command Code models through CLIProxyAPI | [auth-commandcode/README.md](auth-commandcode/README.md) |

## Use

The host needs:

- A CLIProxyAPI binary built with CGO (`X-CPA-SUPPORT-PLUGIN: 1` on management responses)
- A management key (`remote-management.secret-key`)
- An **absolute writable** `plugins.dir`. Under Homebrew / launchd / systemd, a relative `"plugins"` or `~` is resolved against `/`

```yaml
plugins:
  enabled: true
  dir: "/Users/<you>/.cli-proxy-api/plugins"
  store-sources:
    - "https://raw.githubusercontent.com/UNICKCHENG/cliproxyapi-plugins/main/registry.json"
```

Restart CLIProxyAPI, then install:

```bash
curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  "localhost:8317/v0/management/plugin-store/auth-cursor/install"
```

Store install does not flip `plugins.enabled`; set that yourself. Credentials, models, and plugin options live in the plugin README.

## Develop

Each plugin has its own `Makefile` and README.

```bash
cd auth-cursor
make test
make build          # dist/auth-cursor.<ext>
```

The Command Code plugin is the same shape:

```bash
cd auth-commandcode
make test
make build          # dist/auth-commandcode.<ext>
```

How to load a local build, bump dependencies, and cut a release: see the plugin README.

Release tags are `<plugin>-v<version>` (for example `auth-cursor-v2.0.0`). CI publishes platform archives and updates `registry.json`.

## If install fails

- `create plugin directory: mkdir plugins: read-only file system`: set `plugins.dir` to an absolute path.
- No `X-CPA-SUPPORT-PLUGIN` on management responses: this binary does not support dynamic plugins.
- The plugin installed but behaves wrong: the plugin README's troubleshooting section.

## License

[MIT](LICENSE)
