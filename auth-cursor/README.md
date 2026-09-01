# auth-cursor

Exposes Cursor models on CLIProxyAPI's OpenAI, Claude, and Gemini compatible endpoints.

Two names, not interchangeable:

| | Value | Used for |
| --- | --- | --- |
| Plugin ID | `auth-cursor` | Store, `plugins.configs`, library filename |
| Provider key | `cursor` | Auth files, `--cursor-login`, `owned_by` on `/v1/models`, `oauth-model-alias` / `oauth-excluded-models` |

Cursor has no chat-completions HTTP API and no official Go client. Calls go through the [SDK Bridge](https://cursor.com/docs/sdk/bridge): a local process that embeds the TypeScript SDK and speaks `sdk.v1`. This plugin is the C-ABI library the host loads. It starts the bridge, completes the handshake, and forwards requests. On first use it downloads a pinned, checksummed bridge into the user cache. **No Node.js.**

```
client (OpenAI / Claude / Gemini)
  -> host translates to chat-completions
    -> auth-cursor (C ABI)
      -> cursor-sdk-bridge (sdk.v1 over loopback HTTP/1.1)
        -> Cursor; through the plugin's loopback egress when a proxy is set
```

A full host config is in [config.example.yaml](config.example.yaml).

## Use

Store source and `plugins.dir` are in the [repository README](../README.md). This section is what this plugin adds on top.

You need a Cursor API key from the [dashboard](https://cursor.com/dashboard) (personal or a team service-account key). First run also needs GitHub Releases, to fetch the bridge.

```yaml
plugins:
  configs:
    auth-cursor:
      enabled: true
      bridge-path: ""     # empty = download the pinned cursor-sdk-bridge on first use
      proxy-url: ""       # host-level proxy-url does not apply to Cursor traffic
      optimize-for: "balanced"
      weights: {}
```

Restart, install, and check that it actually registered:

```bash
curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  localhost:8317/v0/management/plugin-store

curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  "localhost:8317/v0/management/plugin-store/auth-cursor/install"

curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  localhost:8317/v0/management/plugins
```

Expect `"registered": true` and `"effective_enabled": true`. The store writes a **versioned** filename, for example `plugins/darwin/arm64/auth-cursor-v2.0.0.dylib`. That is what the host loads, not `auth-cursor.dylib`.

Import a key and send a request:

```bash
./cli-proxy-api --cursor-login
# no TTY (brew services, redirect):
# ./cli-proxy-api --cursor-login --cursor-api-key "key_..."

./cli-proxy-api &
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/models
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/chat/completions \
  -d '{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}'
```

The first request after install or upgrade downloads `cursor-sdk-bridge` (tens of MB, once per bridge version). Later runs reuse the cache. If `--cursor-login` sits with no prompt, it is usually this download.

## Configure

`plugins.configs.auth-cursor`:

| Option | Purpose |
| --- | --- |
| `bridge-path` | The `cursor-sdk-bridge` executable, or the directory holding it. Empty means auto-download |
| `proxy-url` | Proxy used when the auth file has no `proxy_url` |
| `optimize-for` | Router mode for `auto-smart`: `cost`, `balanced`, or `intelligence` |
| `weights` | Per-credential share for weighted routing, keyed by account email or auth file name |

Model aliases and exclusions are **host-level**, keyed by `cursor`, not inside `plugins.configs`. See [Models](#models).

### Bridge

Lookup order, first match wins:

1. `CURSOR_SDK_BRIDGE_BIN` (absolute path to the executable)
2. `bridge-path` in config (file or its directory)
3. The unpacked cache at `<cache>/cli-proxy-api/auth-cursor-bridge/<bridge-version>/` (`~/Library/Caches` on macOS, `~/.cache` or `$XDG_CACHE_HOME` on Linux, `%LocalAppData%` on Windows)
4. Download of the pinned GitHub release

The checksum is compiled into the plugin, not taken from the file next to the archive. A mismatch aborts and leaves nothing behind. If GitHub is unreachable, download `cursor-sdk-bridge-standalone-<platform>.tar.gz` from [cursor/sdk-bridge releases](https://github.com/cursor/sdk-bridge/releases), unpack it, and point `bridge-path` at it.

One bridge process per resolved proxy, shared by every credential that uses that proxy. Each credential gets a private empty workspace. Agent state lives under `<cache>/cli-proxy-api/auth-cursor-bridge/<plugin-version>/state/`.

### Credentials

There is no browser sign-in. `sdk.v1` has no login endpoint; create the key in the dashboard.

```bash
./cli-proxy-api --cursor-login
./cli-proxy-api --cursor-login --cursor-api-key "key_..."
```

The key is checked against Cursor before anything is saved. The email that check returns names the file `cursor-<account>.json`; importing the same account replaces that file. The command exits without starting the server. Prefer the prompt when a person is present: `--cursor-api-key` is visible in the process list and shell history. The import never reads `CURSOR_API_KEY`.

Or write the file yourself. `type` is `cursor`, not `auth-cursor`:

```json
{
  "type": "cursor",
  "api_key": "key_..."
}
```

Optional fields: `label`, `prefix`, `proxy_url`, `disabled`, `note`, `model_aliases`, `excluded-models`. Re-running `--cursor-login` keeps those, and any other custom field.

Dashboard keys have no expiry the plugin can read. A key revoked in the dashboard starts failing with 401 at request time. There is no `cursor-api-key` config field; keys live in `auth-dir`.

### Load balancing

One file per key. To weight accounts, set `routing.strategy: weighted-round-robin` and list shares in the plugin config:

```yaml
routing:
  strategy: "weighted-round-robin"

plugins:
  configs:
    auth-cursor:
      weights:
        you@example.com: 5
        teammate@example.com: 2
        cursor-service-account.json: 1
```

Keys match the credential `email` first, then the auth file name with and without `.json`, case-insensitively. Omitted credentials keep share 1. `0` or a negative value parks the credential. The maximum is 1000000.

Do not also set `weight` in the auth file: `--cursor-login` rewrites that file, and a leftover file-level `weight` silently wins. Editing `weights` takes effect on the host's normal config reload.

### Proxying

The bridge reaches Cursor itself. **Host-level `proxy-url` has no effect** on that traffic. Set `plugins.configs.auth-cursor.proxy-url`, or `proxy_url` in the auth file.

Per credential: auth-file `proxy_url` first, then the plugin `proxy-url`. One bridge process per resolved proxy. A proxy already in the host environment is used when the plugin has none of its own.

The bridge's HTTP agent ignores `HTTP_PROXY`, so the plugin starts a loopback reverse proxy next to each bridge and dials out in Go, including `socks5://`. Skipped when there is no proxy. Image URLs are fetched through the host HTTP client and sent as inline bytes.

The egress binds `[::1]`, then `127.0.0.2` if that is missing. It will not bind `127.0.0.1`, because the SDK turns off certificate verification for that backend URL. Linux without IPv6 still works via `127.0.0.2`. A stock macOS has `[::1]` but not `127.0.0.2`, so leave IPv6 enabled for a proxied deployment.

### Models

Availability is per account. The plugin asks Cursor for the catalog with each credential. Whatever that key can reach shows up on `/v1/models`: Composer, Grok, and the OpenAI / Anthropic / Google models in the Cursor usage pool. Cursor Router appears as `auto-smart` when the team has it enabled. Some catalogs also include a legacy `default` id.

`optimize-for` is applied to `auto-smart`. Other per-model parameters are dropped when the selected model does not expose them. Matching is by id only. A failed discovery publishes nothing for that key; generation still forwards the requested id and Cursor decides.

Aliases and exclusions are host-level, keyed by `cursor`:

```yaml
oauth-model-alias:
  cursor:
    - name: "grok-4.6"      # upstream id reported by Cursor
      alias: "grok-latest"  # id your clients use
      # fork: true          # also keep the upstream id on /v1/models
      # display-name: "Grok Latest"

oauth-excluded-models:
  cursor:
    - "default"
```

A value nested under `plugins.configs.auth-cursor` is ignored. Exclusions match model ids case-insensitively; `*` is a substring wildcard. `default` and `auto-smart` are different ids — hiding Auto/Router requires `- "auto-smart"` as well.

Per account: put `model_aliases` / `excluded-models` in that auth file. The per-account list is consulted first. After editing, let the host reload or restart.

## Behaviour

This is an agent SDK, not a raw inference API. Claude or GPT selections still run through the Cursor agent harness and bill the Cursor usage pool.

A text-only conversation that continues from the last assistant turn is sent as an increment on the same `agent_id` when the credential, model, and tool set match. A miss, a concurrent request with the same key, or a failed/cancelled turn starts a new agent. If reuse cannot be proven, the plugin flattens the full message list into one user prompt — larger than the same dialogue on a native API. An over-budget send returns **400** `context_length_exceeded` without cooling the credential.

Tools stay off unless the client sends them. The working directory is always empty. Client `tools` are registered as `local.custom_tools` and need an mcp allowlist. Dropping the stream does not stop Cursor; the plugin issues an explicit cancel, then deletes the agent.

Other limits:

- No token-counting endpoint. `count_tokens` is a character estimate; billing uses counts Cursor reports after a run.
- `executor.http_request` returns 501.
- Remote images: only `http`/`https` to a non-loopback, non-private, non-metadata host. Credentials in the URL are refused. The error text never repeats the path or query. The host HTTP client still follows redirects, so a public URL that 302s onto loopback is not blocked here.
- Region/plan refusals fail that one request without parking the credential. A 401 or 429 that arrives as an `SdkErrorCode` still cools or retires the key.
- Host usage statistics report provider `cursor`. `request-log: true` records the client side; the upstream HTTP section stays empty because the call goes over Connect.

```bash
grep 'cursor request' ~/.cli-proxy-api/logs/main.log
```

A completed line carries `auth_id`, `auth_label`, `model`, `stream`, `latency_ms`, token counts, and for streams `ttft_ms`. A failed one carries `failed=true`. The prompt, response, and API key are never logged.

Every request bills a real Cursor account. Do not share one account's key, and do not resell or publicly redistribute Cursor access obtained through this plugin. Expect rate limiting under automated load. Review Cursor's current terms before deploying; this is not legal advice.

## Develop

Needs a CGO-capable Go toolchain. Generated `sdk.v1` clients are committed; building does not need `buf` unless you regenerate.

```bash
make fmt
make test
make build                        # dist/auth-cursor.<ext>
make proto                        # only when the pinned bridge release changes
```

Tests use an in-process fake `sdk.v1` service: no Cursor key, no bridge, no network. Live handshake:

```bash
cd go && CURSOR_SDK_BRIDGE_BIN=/path/to/cursor-sdk-bridge \
  go test -run TestLiveBridgeHandshake ./...
```

### Load a local build

`make install INSTALL_DIR=/path/to/plugins` copies **only** `auth-cursor.<ext>`. After a store install the host loads the **versioned** name. Overwrite both:

```bash
make build
cp dist/auth-cursor.dylib ~/.cli-proxy-api/plugins/darwin/arm64/auth-cursor.dylib
cp dist/auth-cursor.dylib ~/.cli-proxy-api/plugins/darwin/arm64/auth-cursor-v2.0.0.dylib
chmod +x ~/.cli-proxy-api/plugins/darwin/arm64/auth-cursor*.dylib
brew services restart cliproxyapi
```

Confirm in `~/.cli-proxy-api/logs/main.log`:

```
pluginhost: plugin loaded plugin_id=auth-cursor version=0.0.0-dev
# or path=.../auth-cursor-v2.0.0.dylib
```

A `make build` without `-ldflags` reports `0.0.0-dev`. Re-installing from the store overwrites the local library. Wiping `plugins/` and copying only `auth-cursor.dylib` is not enough if the config still records a store install of `auth-cursor` 2.0.0 — the host looks for `auth-cursor-v2.0.0.dylib`.

### Bump the bridge

Replace `proto/`, update `bridgeVersion` and the checksums in `go/bridge_release.go`, run `make proto`. Those three move together.

Also re-check `CURSOR_BACKEND_URL` and the default Cursor hosts in `go/bridge_egress.go` against strings in the new bridge executable.

## Troubleshooting

**`oauth-excluded-models` looks right, but `default` is still on `/v1/models`.**

The block must sit at host root, provider key `cursor`, not under `plugins.configs.auth-cursor`. Store 2.0.0 did not filter `/v1/models`; check that `plugin loaded` in `main.log` names the library you just copied. `auto-smart` is a different id — exclude it separately to hide Router.

**I copied `dist/auth-cursor.dylib` and restarted. Nothing changed.**

Check `plugin loaded` and the **path**. If it still names `auth-cursor-v2.0.0.dylib`, overwrite that file too. Confirm `plugins.dir` is the directory you copied into (Homebrew: `~/.cli-proxy-api/plugins`).

**`/v1/models` has no Cursor entries.**

The library never loaded (wiped `plugins/` without restoring the versioned filename), discovery failed (`cursor model discovery failed` in the log), or registrar timed out (`pluginhost: model registrar auth-cursor failed: context deadline exceeded`).

**Plugin missing from `/v0/management/plugins`, or `"registered": false`.**

`plugins.enabled` is off, `plugins.dir` does not resolve, or the host binary lacks CGO plugin support.

**`create plugin directory: mkdir plugins: read-only file system`**

Relative `plugins.dir` or a literal `~` under brew services / launchd. Set `/Users/<you>/.cli-proxy-api/plugins`. [CLIProxyAPI #4313](https://github.com/router-for-me/CLIProxyAPI/issues/4313).

**Bridge download / sha256 mismatch / bridge not ready.**

The host cannot reach GitHub, a proxy rewrote the archive, or `bridge-path` points at the wrong binary. Download `cursor-sdk-bridge-standalone-<platform>.tar.gz` elsewhere and set `bridge-path`. `cursor sdk bridge stderr` is the bridge's own output.

**`cursor login failed: could not reach the cursor api`**

Host-level `proxy-url` does not apply. Set `plugins.configs.auth-cursor.proxy-url` and confirm with `curl -x <proxy> -I --max-time 15 https://api2.cursor.sh`.

**`Model not available … not supported in your region`**

That one model is refused; the credential keeps serving the rest. Hide those ids with `oauth-excluded-models.cursor` or per-account `excluded-models`.

**Every Cursor request becomes `503 auth_unavailable` after one failure.**

An unclassified upstream error parked the credential. The `upstream execution failed` line names the cause. `disable-cooling: true` is an emergency escape.

**`--cursor-login` has no prompt.**

First run is downloading the bridge. If stdin is not a TTY (brew services, redirect), pass `--cursor-api-key`.

**Hundreds of `auth-cursor-sidecar/.../package.json` auth files.**

Plugin 1.0.0 and earlier unpacked a Node sidecar into the default `auth-dir`. This version does not. Delete `~/.cli-proxy-api/auth-cursor-sidecar/` by hand.
