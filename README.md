# CLIProxyAPI plugins

Third-party plugins for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI),
distributed as a plugin store source you add to your own configuration.

## Plugins

| ID | Description | Docs |
| --- | --- | --- |
| `auth-cursor` | Call Cursor models through the official Cursor SDK Bridge. | [auth-cursor/](auth-cursor/) |

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
- A Cursor account, and an API key created in the
  [Cursor dashboard](https://cursor.com/dashboard) (personal) or by your team admin
  (service account).
- Network access to GitHub releases on first use, for the bridge download described below.

No Node.js or npm is needed. Earlier versions ran a Node sidecar; this one does not.

### Bridge bootstrap (automatic)

Cursor's SDK is a TypeScript agent SDK with no Go client, so the plugin drives the official
[SDK Bridge](https://cursor.com/docs/sdk/bridge) instead: a local `cursor-sdk-bridge` process
that the plugin speaks `sdk.v1` Connect/protobuf to.

The store installs only the dynamic library (`auth-cursor.dylib` / `.so` / `.dll`). On the
**first use** after an install or upgrade — `--cursor-login`, the first server start, or the
first request — the plugin downloads the pinned bridge release, verifies it against a
SHA-256 compiled into the library, and unpacks it into the user cache directory,
`<cache>/cli-proxy-api/auth-cursor-bridge/<bridge-version>/` (`~/Library/Caches` on macOS,
`~/.cache` or `$XDG_CACHE_HOME` on Linux, `%LocalAppData%` on Windows).

The cache directory is deliberate: the default `auth-dir` is `~/.cli-proxy-api`, and anything
left there is scanned as a credential candidate.

Leave `bridge-path` empty for this. If the host cannot reach GitHub, download the archive
elsewhere and point `bridge-path` at the unpacked tree — see
[auth-cursor/README.md](auth-cursor/README.md#the-bridge-binary).

### Steps

**1. Add this repository as a plugin store source and restart CLIProxyAPI**

Store source URLs must be `https`; the host rejects `http` and `file` URLs.

```yaml
plugins:
  enabled: true
  dir: "plugins"          # Homebrew/launchd/systemd: use an absolute writable path, not "plugins" or "~"
  store-sources:
    - "https://raw.githubusercontent.com/UNICKCHENG/cliproxyapi-plugins/main/registry.json"
  configs:
    auth-cursor:
      enabled: true
      bridge-path: ""     # empty downloads the pinned cursor-sdk-bridge on first use
      proxy-url: ""       # see step 5
      optimize-for: "balanced"
      weights: {}         # per-credential share for weighted routing
```

A full copy-paste example lives in [auth-cursor/config.example.yaml](auth-cursor/config.example.yaml).

Two things worth knowing before you tune anything: renaming or hiding Cursor models uses the
host-level `oauth-model-alias` / `oauth-excluded-models` blocks under the provider key
`cursor`, and giving one account a larger share of traffic uses `weights` above together with
`routing.strategy: weighted-round-robin`. Both are covered in
[auth-cursor/README.md](auth-cursor/README.md#load-balancing).

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

**5. Set a proxy if the host needs one**

The bridge connects to Cursor itself rather than through the host's HTTP client, so the
host-level `proxy-url` does not apply to it. Restate it for the plugin, or per credential
with `proxy_url` in the auth file:

```yaml
plugins:
  configs:
    auth-cursor:
      proxy-url: "http://127.0.0.1:7890"   # or socks5://…
```

A proxy already exported into the host's environment is inherited when this is empty, so
skip this step if that is how the machine is set up.

**6. Import your Cursor API key**

Create the key in the [Cursor dashboard](https://cursor.com/dashboard) first, then:

```bash
./cli-proxy-api --cursor-login
Paste your Cursor API key (https://cursor.com/dashboard): key_...
```

The first run after an install or upgrade downloads `cursor-sdk-bridge` before the prompt
appears — tens of megabytes, once per bridge version.

The key is checked against Cursor before anything is written, so a typo fails here rather
than at the first request. The account it belongs to names the file, `cursor-<account>.json`
in `auth-dir`, and the command then exits without starting the server. Re-running it replaces
the key and keeps the settings you added to that file.

There is no browser sign-in. For scripts, `--cursor-login --cursor-api-key "key_..."` skips
the prompt; you can also write the auth file yourself. Both are covered in
[auth-cursor/README.md](auth-cursor/README.md#credentials).

**7. Start the server and verify**

```bash
./cli-proxy-api &
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/models
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/chat/completions \
  -d '{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}'
```

If `--cursor-login` already downloaded the bridge, these requests are fast. Otherwise the
first one waits for that download.

### Common install issues

| Symptom | What to check |
| --- | --- |
| Plugin missing from `/v0/management/plugins` | `plugins.enabled` is off, or `plugins.dir` does not resolve |
| `"registered": false` | The library failed to load; the binary may lack CGO plugin support |
| `create plugin directory: mkdir plugins: read-only file system` (or `mkdir ~: …`) | Relative `plugins.dir` (`"plugins"`) or a literal `~` is resolved against the service CWD, often `/` under Homebrew `brew services` / launchd. Set an absolute writable path such as `/Users/<you>/.cli-proxy-api/plugins` and restart. Same class of bug: [CLIProxyAPI #4313](https://github.com/router-for-me/CLIProxyAPI/issues/4313). |
| `download …cursor-sdk-bridge-standalone-…tar.gz` fails | The host cannot reach GitHub releases. Fix the network path, set `proxy-url`, or install the bridge by hand and set `bridge-path`. |
| `verify …: sha256 mismatch` | The archive is not the pinned release — a proxy or mirror rewrote it. Nothing was installed. |
| `cursor sdk bridge exited before it was ready` | The bridge binary cannot run here; check the message it printed, or set `bridge-path` to a bridge you installed. |
| `/v1/models` has no Cursor entries | Discovery failed for that credential; check the `cursor model discovery failed` log line |
| `--cursor-login` sits with no prompt on first run | Normal: the pinned bridge is downloading. Later runs reuse `<cache>/cli-proxy-api/auth-cursor-bridge/<bridge-version>/`. |
| `no terminal to prompt on` from `--cursor-login` | stdin is not a terminal (a service manager, or a redirect). Pass `--cursor-api-key` instead. |
| Hundreds of `auth-cursor-sidecar/.../package.json` entries in the auth file list, and `DELETE` on them returns 400 | Plugin version 1.0.0 and earlier bootstrapped a Node sidecar inside the default `auth-dir`. This version runs no sidecar; remove `~/.cli-proxy-api/auth-cursor-sidecar/` by hand. |

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

`make install` copies only the library. The bridge is fetched into the user cache directory on
first use, or taken from `bridge-path` / `CURSOR_SDK_BRIDGE_BIN` if you already have one.

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
