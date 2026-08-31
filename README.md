# CLIProxyAPI plugins

Third-party plugins for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI),
distributed as a plugin store source you add to your own configuration.

## Plugins

| ID | Description | Docs |
| --- | --- | --- |
| `auth-cursor` | Call Cursor models through the official `@cursor/sdk` agent SDK. | [auth-cursor/](auth-cursor/) |

<a id="install-auth-cursor"></a>

## Install guide (auth-cursor)

Follow this section once, from adding the store source through the first successful request.
Configuration details, model behaviour, and troubleshooting live in
[auth-cursor/README.md](auth-cursor/README.md).

### Prerequisites

- CLIProxyAPI built with CGO. Management responses carry `X-CPA-SUPPORT-PLUGIN: 1` when the
  running binary supports dynamic library plugins.
- A management key configured (`remote-management.secret-key`), because plugins are
  installed through the management API.
- **Node.js 22.13 or later and `npm` on the host that runs CLIProxyAPI.** The plugin runs a
  Node sidecar for every request. The store install does not bundle Node for you.
- A Cursor account.

### Sidecar bootstrap (automatic, but Node is required)

The store installs only the dynamic library (`auth-cursor.dylib` / `.so` / `.dll`). Sidecar
scripts are embedded in that library. On the **first request** after an install or upgrade the
plugin:

1. writes the scripts into `~/.cli-proxy-api/auth-cursor-sidecar/<version>/`;
2. runs `npm install --omit=dev` there once to fetch `@cursor/sdk`.

You do **not** need to copy sidecar files or run `npm install` yourself, and you can leave
`sidecar-path` empty. You **do** need Node and npm on the machine, and the first request
needs network access to the npm registry, so it is slower than later ones.

### Steps

**1. Add this repository as a plugin store source and restart CLIProxyAPI**

Store source URLs must be `https`; the host rejects `http` and `file` URLs.

```yaml
plugins:
  enabled: true
  dir: "plugins"          # use an absolute path under launchd or systemd
  store-sources:
    - "https://raw.githubusercontent.com/UNICKCHENG/cliproxyapi-plugins/main/registry.json"
  configs:
    auth-cursor:
      enabled: true
      priority: 1
      node-path: "node"   # see step 5 if you run under a service manager
      sidecar-path: ""
      optimize-for: "balanced"
      models: []
```

A full copy-paste example lives in [auth-cursor/config.example.yaml](auth-cursor/config.example.yaml).

**2. Confirm the source is readable**

```bash
curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  localhost:8317/v0/management/plugin-store
```

The response should list this repository under `sources` and must not mention it under
`source_errors`.

**3. Install the plugin**

```bash
curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  "localhost:8317/v0/management/plugin-store/auth-cursor/install"
```

This downloads the platform archive, verifies its SHA-256 against `registry.json`, writes the
library into `plugins/<GOOS>/<GOARCH>/`, and adds the `auth-cursor` config block. It never
flips the global `plugins.enabled` — set that yourself in step 1.

**4. Confirm the plugin loaded**

```bash
curl -H "Authorization: Bearer $MANAGEMENT_KEY" localhost:8317/v0/management/plugins
```

Expect `"registered": true` and `"effective_enabled": true`. If `registered` is false, check
that `plugins.dir` resolves (use an absolute path under a service manager) and that the binary
reports `X-CPA-SUPPORT-PLUGIN: 1`.

**5. Set `node-path` when running as a service**

Interactive shells usually resolve `node-path: "node"` from PATH. Service managers
(launchd, systemd, brew services) often expose a minimal PATH, so set an absolute path:

```yaml
plugins:
  configs:
    auth-cursor:
      node-path: "/opt/homebrew/bin/node"   # or your mise / nvm / asdf path
```

`npm` is looked up next to `node-path` before PATH, so a version-manager install works even
when the service PATH does not expose it.

**6. Sign in to Cursor**

```bash
./cli-proxy-api --cursor-login              # opens the browser
./cli-proxy-api --cursor-login --no-browser # prints the URL instead
```

This mints a user API key and writes `cursor-<account>.json` into `auth-dir`, then exits
without starting the server. See [auth-cursor/README.md](auth-cursor/README.md#credentials)
for dashboard keys and expiry behaviour.

**7. Start the server and verify**

```bash
./cli-proxy-api &
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/models
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/chat/completions \
  -d '{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}'
```

The first `/v1/models` or chat request after an install may take up to a minute while the
sidecar bootstraps and runs `npm install`. Later requests reuse
`~/.cli-proxy-api/auth-cursor-sidecar/<version>/`.

### Common install issues

| Symptom | What to check |
| --- | --- |
| Plugin missing from `/v0/management/plugins` | `plugins.enabled` is off, or `plugins.dir` does not resolve |
| `"registered": false` | The library failed to load; the binary may lack CGO plugin support |
| `no such file or directory` naming `node` | Set an absolute `node-path` (step 5) |
| `/v1/models` returns only configured `models` | Sidecar failed to start — usually a missing or wrong `node-path` |
| First request hangs ~1 minute | Normal: sidecar bootstrap is running `npm install` |

More detail: [auth-cursor/README.md — Troubleshooting](auth-cursor/README.md#troubleshooting).

## Plugin IDs and provider keys are different

A plugin's **ID** is its distribution name — the store API, the `plugins.configs` key, and
the library file name (`auth-cursor.dylib` → id `auth-cursor`). The host derives the id from
that file name, so it always matches `registry.json`.

A plugin's **provider key** is what credentials and models use at runtime — the `type` field
in an auth file, and `owned_by` on `/v1/models`. These do not have to match the plugin id.

`auth-cursor` is configured under `plugins.configs.auth-cursor`, but credentials stay on the
`cursor` provider key:

```yaml
plugins:
  configs:
    auth-cursor:      # plugin ID
      enabled: true
```

```json
{ "type": "cursor", "api_key": "key_..." }
```

## Repository layout

```
registry.json                 # plugin store source, updated by the release workflow
scripts/update-registry.mjs   # regenerates one registry entry from release archives
scripts/validate-registry.mjs # checks registry.json against the host's constraints
<plugin>/plugin.json          # static store metadata for that plugin
<plugin>/                     # plugin sources, Makefile and documentation
```

## Building from source

Use this when you want an unreleased change, or a platform the release workflow does not
cover. You still need the config block from the install guide above.

```bash
cd auth-cursor
make build                        # dist/auth-cursor.<ext>
make test
make install INSTALL_DIR=/path/to/CLIProxyAPI/plugins
```

`make install` also places a ready-to-run sidecar at
`plugins/<GOOS>/<GOARCH>/auth-cursor-sidecar/`, which the plugin prefers over the home
bootstrap directory, so a development host never runs `npm install` at request time.

## Releasing

Plugins version independently. Tag `<plugin>-v<version>` (for example `auth-cursor-v0.1.0`)
and the release workflow builds every supported platform on a native runner, publishes the
archives with `checksums.txt`, then rewrites that plugin's `registry.json` entry with the
artifact URLs and checksums.

The registry uses schema version 2 with the `direct` install type. That is deliberate: the
`github-release` install type resolves a repository's single latest release, so in a
multi-plugin repository one plugin's release would hide every other plugin. Pinning artifact
URLs per version keeps the plugins independent.

## License

[MIT](LICENSE)
