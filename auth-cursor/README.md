# auth-cursor

Exposes Cursor models through CLIProxyAPI's OpenAI, Claude and Gemini compatible
endpoints.

Two names matter here and they are not the same:

- the **plugin ID** is `auth-cursor` — store API, `plugins.configs` key, library
  file stem;
- the **provider key** is `cursor` — auth files (`"type": "cursor"`),
  `--cursor-login`, `owned_by` on `/v1/models`, and host blocks such as
  `oauth-model-alias` / `oauth-excluded-models`.

Cursor's SDK is an *agent* SDK, not a chat-completions API, and Cursor publishes
no Go client for it. Instead it publishes the
[SDK Bridge](https://cursor.com/docs/sdk/bridge): a local process that embeds
the TypeScript SDK and exposes it over the `sdk.v1` Connect/protobuf contract.
The plugin ships:

- a C-ABI shared library (`auth-cursor.dylib` / `.so` / `.dll`) that the host
  loads, declaring `executor`, `auth_provider`, `model_provider` and
  `command_line_plugin`;
- generated `sdk.v1` clients, and the process management that starts
  `cursor-sdk-bridge`, completes its handshake, and shuts it down.

The bridge binary is not bundled. On first use the plugin downloads the pinned
release, verifies a checksum compiled into the library, and unpacks it into the
user cache. **No Node.js or npm** — earlier versions ran a Node sidecar; this
one does not.

Each request creates a one-shot agent with an empty working directory and an
empty tool list, so the agent can only answer with text. That reduces an agent
run to plain model inference, which is what a proxy request means.

```
client (OpenAI/Claude/Gemini)
  -> host translates to chat-completions
    -> auth-cursor plugin (C ABI)
      -> cursor-sdk-bridge (sdk.v1 over Connect, loopback HTTP/1.1)
        -> Cursor backend, through the plugin's loopback egress when a proxy is configured
```

## Install

### Prerequisites

- CLIProxyAPI built with CGO. Management responses carry
  `X-CPA-SUPPORT-PLUGIN: 1` when the binary supports dynamic plugins.
- A management key (`remote-management.secret-key`). Plugins are installed
  through the management API.
- A Cursor API key from the [Cursor dashboard](https://cursor.com/dashboard)
  (personal) or a team service-account key.
- Network access to GitHub releases on first use, for the bridge download.

### Store install

Store source URLs must be `https`. Under Homebrew / launchd / systemd, use an
**absolute** `plugins.dir` — relative `"plugins"` or `~` is resolved against
`/`, which is read-only.

```yaml
plugins:
  enabled: true
  dir: "/Users/<you>/.cli-proxy-api/plugins"
  store-sources:
    - "https://raw.githubusercontent.com/UNICKCHENG/cliproxyapi-plugins/main/registry.json"
  configs:
    auth-cursor:
      enabled: true
      bridge-path: ""     # empty downloads the pinned cursor-sdk-bridge on first use
      proxy-url: ""       # host-level proxy-url does not apply to Cursor traffic
      optimize-for: "balanced"
      weights: {}
```

A full copy-paste host config is in [config.example.yaml](config.example.yaml).
Restart the host, then:

```bash
# Confirm the source is listed and not in source_errors
curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  localhost:8317/v0/management/plugin-store

curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  "localhost:8317/v0/management/plugin-store/auth-cursor/install"

curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  localhost:8317/v0/management/plugins
```

Expect `"registered": true` and `"effective_enabled": true`. The store writes a
**versioned** library, for example
`plugins/darwin/arm64/auth-cursor-v2.0.0.dylib`. That is the file the host
loads after a store install — not `auth-cursor.dylib`.

The install never flips `plugins.enabled`; set that yourself.

### First request

```bash
./cli-proxy-api --cursor-login
Paste your Cursor API key (https://cursor.com/dashboard): key_...

./cli-proxy-api &
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/models
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/chat/completions \
  -d '{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}'
```

The first run after an install or upgrade downloads `cursor-sdk-bridge` before
the prompt — tens of megabytes, once per bridge version. Later runs reuse
`<cache>/cli-proxy-api/auth-cursor-bridge/<bridge-version>/`.

## Configure

The plugin-owned block is `plugins.configs.auth-cursor`:

| Option | Purpose |
| --- | --- |
| `bridge-path` | The `cursor-sdk-bridge` executable, or the directory holding it. Empty means the automatic download. |
| `proxy-url` | Proxy for credentials whose auth file has no `proxy_url`. See [Proxying](#proxying). |
| `optimize-for` | Router mode for `auto-smart`: `cost`, `balanced`, or `intelligence`. |
| `weights` | Per-credential share for weighted routing, keyed by account email or auth file name. |

Model aliases and exclusions are **host-level**, keyed by `cursor`, not by the
plugin ID, and **not** inside `plugins.configs`. See [Models](#models).

### The bridge binary

Lookup order, most explicit first:

1. `CURSOR_SDK_BRIDGE_BIN`, an absolute path to the executable;
2. `bridge-path` from the config, either the executable or the directory holding it;
3. the copy already unpacked at
   `<cache>/cli-proxy-api/auth-cursor-bridge/<bridge-version>/bin/cursor-sdk-bridge`
   (`~/Library/Caches` on macOS, `~/.cache` or `$XDG_CACHE_HOME` on Linux,
   `%LocalAppData%` on Windows);
4. a download of the pinned release from GitHub.

The download is verified against a SHA-256 that ships inside the plugin, not
against the checksum file published beside the archive. A mismatch aborts and
leaves nothing behind.

Pinned to one bridge version deliberately — the `sdk.v1` contract the plugin is
generated against comes from that same release. If GitHub is unreachable,
download `cursor-sdk-bridge-standalone-<platform>.tar.gz` from
[cursor/sdk-bridge releases](https://github.com/cursor/sdk-bridge/releases),
unpack it, and point `bridge-path` at it.

One bridge process is started per proxy, shared by every credential that uses
it, and each gets a private empty workspace. Durable agent state lives under
`<cache>/cli-proxy-api/auth-cursor-bridge/<plugin-version>/state/`; the plugin
deletes each one-shot agent after its request.

## Credentials

A Cursor credential is an API key you create yourself. There is no browser
sign-in — `sdk.v1` exposes no login endpoint.

```bash
./cli-proxy-api --cursor-login
# scripts / brew services (no TTY):
./cli-proxy-api --cursor-login --cursor-api-key "key_..."
```

The key is checked against Cursor before anything is saved. The account email
that check returns names the file — `cursor-<account>.json` in `auth-dir` —
so importing a key for the same account replaces that file. The command exits
without starting the server.

Prefer the prompt when a person is present: an argument is visible in the
process list and in shell history. The import never reads `CURSOR_API_KEY`.

Re-importing keeps settings the plugin does not own: `label`, `prefix`,
`proxy_url`, `disabled`, `note`, `model_aliases`, `excluded-models`, and any
custom field.

You can also write the file yourself:

```json
{
  "type": "cursor",
  "api_key": "key_..."
}
```

`type` is `cursor`, not `auth-cursor`. Optional fields: `label`, `prefix`,
`proxy_url`, `disabled`, `note`, `model_aliases`, `excluded-models`.

Dashboard keys carry no expiry the plugin can read. A key revoked in the
dashboard starts failing with 401 at request time. There is no
`cursor-api-key` config field — keys live in `auth-dir`.

### Load balancing

One file per key. To weight accounts, set `routing.strategy:
weighted-round-robin` and list shares in the plugin config:

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

Keys match the credential `email` first, then the auth file name with and
without `.json`, case-insensitively. Omitted credentials keep share 1. `0` or
a negative value parks the credential. The maximum is 1000000.

Do not also set `weight` in the auth file: `--cursor-login` rewrites that file,
and a leftover file-level `weight` silently wins. Editing `weights` takes
effect on the host's normal config reload.

## Proxying

The bridge reaches Cursor itself. The **host-level `proxy-url` has no effect**
on that traffic — set `plugins.configs.auth-cursor.proxy-url`, or `proxy_url`
in the auth file.

Per credential: auth-file `proxy_url` first, then the plugin `proxy-url`. One
bridge process per resolved proxy. A proxy already in the host environment is
used when the plugin has none of its own.

The bridge's HTTP agent ignores `HTTP_PROXY`, so the plugin starts a loopback
reverse proxy next to each bridge and dials out in Go — including `socks5://`.
Skipped when there is no proxy. Image URLs are the exception: they are fetched
through the host HTTP client and sent as inline bytes.

## Models

Availability is per account. The plugin asks Cursor for the catalog with each
credential. Whatever that key can reach shows up on `/v1/models`: Composer,
Grok, and the OpenAI / Anthropic / Google models in the Cursor usage pool.
Cursor Router appears as `auto-smart` when the team has it enabled. Some
catalogs also include a legacy `default` id.

`optimize-for` is applied automatically to `auto-smart`. Other per-model
parameters are dropped when the selected model does not expose them.

Matching is by id only. Discovery is the only source of the list. A failed
discovery publishes nothing for that key; generation still forwards the
requested id and Cursor decides.

### Renaming and hiding models

Both blocks are host-level, keyed by `cursor`, **outside** `plugins.configs`:

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

That YAML is the correct form. It is not plugin config. A value nested under
`plugins.configs.auth-cursor` is ignored.

Exclusions match model **ids** case-insensitively; `*` is a substring wildcard.
`default` and `auto-smart` are different ids — hiding Auto/Router requires
`- "auto-smart"` as well.

Per-account: put `model_aliases` / `excluded-models` in that auth file. The
per-account list is consulted first and wins over the global block.

After editing, let the host reload the config (file watcher) or restart it.
The running plugin must be a build that applies these lists on
`model.static` / `model.for_auth` — store 2.0.0 did not filter `/v1/models`.

## Behaviour and limits

- **Agent semantics, not raw inference.** Claude or GPT selections still run
  through the Cursor agent harness and bill the Cursor usage pool.
- **Stateless.** Every request builds a fresh agent from the full message list.
- **Latency.** Agent creation adds overhead versus a native inference API.
- **A cancelled request cancels the run.** Dropping the stream does not stop
  Cursor; the plugin issues an explicit cancel, then deletes the agent.
- **No token counting endpoint.** `count_tokens` is a character estimate;
  billing uses counts Cursor reports after a run.
- **No raw HTTP passthrough.** `executor.http_request` returns 501.
- **Upstream failures are classified** before the host sees them. Region/plan
  refusals fail that one request without parking the credential.
- **Stream framing depends on the caller.** Chat-completions vs translated
  protocols need different SSE; the executor picks from the inbound path.

## Usage and compliance

The plugin only uses Cursor's published SDK Bridge and keys you create
yourself. Every request still bills a real Cursor account:

- Do not share one account's key with other people.
- Do not resell or publicly redistribute Cursor access obtained through this
  plugin.
- Expect rate limiting under automated load.

Review Cursor's current terms before deploying; this is not legal advice.

## Logging

```bash
grep 'cursor request' ~/.cli-proxy-api/logs/main.log
```

A completed line carries `auth_id`, `auth_label`, `model`, `stream`,
`latency_ms`, token counts, and for streams `ttft_ms`. A failed one carries
`failed=true`. The prompt, response and API key are never logged.

Host usage statistics report provider `cursor` with the credential that served
the call. `request-log: true` records the client side; the upstream HTTP
section stays empty because the call goes over Connect, not the host HTTP
client.

## Development

Needs a CGO-capable Go toolchain. Generated `sdk.v1` clients are committed;
building does not need `buf` unless you regenerate.

```bash
make fmt
make test
make build                        # dist/auth-cursor.<ext>
make proto                        # only when the pinned bridge release changes
```

Tests use an in-process fake `sdk.v1` service: no Cursor key, no bridge, no
network.

```bash
cd go && CURSOR_SDK_BRIDGE_BIN=/path/to/cursor-sdk-bridge \
  go test -run TestLiveBridgeHandshake ./...
```

### Installing a local build

`make install INSTALL_DIR=/path/to/plugins` copies **only**
`auth-cursor.<ext>`. After a store install the host loads the **versioned**
name instead.

Homebrew example (`plugins.dir` = `~/.cli-proxy-api/plugins`, store version
2.0.0):

```bash
make build
# overwrite both names so a store-configured host actually loads the new bits
cp dist/auth-cursor.dylib ~/.cli-proxy-api/plugins/darwin/arm64/auth-cursor.dylib
cp dist/auth-cursor.dylib ~/.cli-proxy-api/plugins/darwin/arm64/auth-cursor-v2.0.0.dylib
chmod +x ~/.cli-proxy-api/plugins/darwin/arm64/auth-cursor*.dylib
brew services restart cliproxyapi
```

Confirm in `~/.cli-proxy-api/logs/main.log`:

```
pluginhost: plugin loaded plugin_id=auth-cursor version=0.0.0-dev
# or, if the store versioned file is what loaded:
pluginhost: plugin loaded ... path=.../auth-cursor-v2.0.0.dylib
```

A `make build` without `-ldflags` reports `0.0.0-dev`. Re-installing from the
store overwrites the local library with the published release.

Wiping `plugins/` and copying only `auth-cursor.dylib` is not enough if the
config still records a store install of `auth-cursor` 2.0.0 — the host looks
for `auth-cursor-v2.0.0.dylib`.

### Bumping the bridge

Replace `proto/`, update `bridgeVersion` and the checksums in
`go/bridge_release.go`, run `make proto`. Those three move together.

Also re-check `CURSOR_BACKEND_URL` and the default Cursor hosts in
`go/bridge_egress.go` against strings in the new bridge executable.

## Q&A

**`oauth-excluded-models` looks right. Why is `default` still on `/v1/models`?**

Three separate things have to be true:

1. The block is at host root, provider key `cursor`, not under
   `plugins.configs.auth-cursor`.
2. The running library actually filters the catalog (this repo does; store
   2.0.0 did not).
3. The host loaded that library. After a store install, copying
   `auth-cursor.dylib` alone does not replace `auth-cursor-v2.0.0.dylib`.

`default` is a catalog id. Cursor Router is `auto-smart` — exclude that id
separately if you want Auto gone too.

**I copied `dist/auth-cursor.dylib` and restarted. Nothing changed.**

Check `main.log` for `plugin loaded` and the **path**. If it still names
`auth-cursor-v2.0.0.dylib`, overwrite that file too (see
[Installing a local build](#installing-a-local-build)). Confirm
`plugins.dir` is the directory you copied into (Homebrew:
`~/.cli-proxy-api/plugins`, not a relative `plugins` next to the binary).

**`/v1/models` has no Cursor entries at all.**

The plugin did not register models. Typical causes: the library never loaded
(wiped `plugins/` without restoring the versioned filename); discovery failed
(`cursor model discovery failed` in the log); or registrar timed out
(`pluginhost: model registrar auth-cursor failed: context deadline exceeded`)
during shutdown/reload.

**Plugin missing from `/v0/management/plugins`, or `"registered": false`.**

`plugins.enabled` is off, `plugins.dir` does not resolve, or the host binary
lacks CGO plugin support.

**`create plugin directory: mkdir plugins: read-only file system`**

Relative `plugins.dir` or a literal `~` under brew services / launchd. Set
`/Users/<you>/.cli-proxy-api/plugins`.
[CLIProxyAPI #4313](https://github.com/router-for-me/CLIProxyAPI/issues/4313).

**Bridge download / sha256 mismatch / bridge not ready.**

The host cannot reach GitHub, a proxy rewrote the archive, or `bridge-path`
points at the wrong binary. Download
`cursor-sdk-bridge-standalone-<platform>.tar.gz` elsewhere and set
`bridge-path`. The log line `cursor sdk bridge stderr` is the bridge's own
output.

**`cursor login failed: could not reach the cursor api`**

Host-level `proxy-url` does not apply. Set `plugins.configs.auth-cursor.proxy-url`
and confirm: `curl -x <proxy> -I --max-time 15 https://api2.cursor.sh`.

**`Model not available … not supported in your region`**

That one model is refused; the credential keeps serving the rest. Hide those
ids with `oauth-excluded-models.cursor` or per-account `excluded-models`.

**Every Cursor request becomes `503 auth_unavailable` after one failure.**

An unclassified upstream error parked the credential. The `upstream execution
failed` line names the cause. `disable-cooling: true` is an emergency escape.

**`--cursor-login` has no prompt.**

First run is downloading the bridge. If stdin is not a TTY (brew services,
redirect), pass `--cursor-api-key`.

**Hundreds of `auth-cursor-sidecar/.../package.json` auth files.**

Plugin 1.0.0 and earlier unpacked a Node sidecar into the default `auth-dir`.
This version does not. Delete `~/.cli-proxy-api/auth-cursor-sidecar/` by hand.
