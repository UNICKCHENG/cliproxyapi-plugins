# auth-cursor

Exposes Cursor models through CLIProxyAPI's OpenAI, Claude and Gemini compatible endpoints.

Two names matter here and they are not the same:

- the **plugin ID** is `auth-cursor` — used for the store API and the `plugins.configs` key;
- the **provider key** is `cursor` — used by credentials (`"type": "cursor"`), the
  `--cursor-login` flag, and `owned_by` on `/v1/models`.

Cursor's SDK is an *agent* SDK, not a chat-completions API, and Cursor publishes no Go
client for it. Instead it publishes the [SDK Bridge](https://cursor.com/docs/sdk/bridge): a
local process that embeds the TypeScript SDK and exposes it over the `sdk.v1`
Connect/protobuf contract. So the plugin ships:

- a C-ABI shared library (`auth-cursor.dylib` / `.so` / `.dll`) that the host loads and that
  declares the `executor`, `auth_provider`, `model_provider` and `command_line_plugin`
  capabilities;
- generated `sdk.v1` clients, and the process management that starts `cursor-sdk-bridge`,
  completes its startup handshake and shuts it down again.

The bridge binary is not bundled. On first use the plugin downloads the pinned release,
verifies it against a checksum compiled into the library, and unpacks it into the user cache
directory. **There is no Node.js or npm prerequisite** — that was the previous
implementation, which ran a Node sidecar.

Each request creates a one-shot agent with an empty working directory and an empty tool
list, so the agent can only answer with text. That reduces an agent run to plain model
inference, which is what a proxy request means.

```
client (OpenAI/Claude/Gemini)
  -> host translates to chat-completions
    -> auth-cursor plugin (C ABI)
      -> cursor-sdk-bridge (sdk.v1 over Connect, loopback HTTP/1.1)
        -> Cursor backend
```

## Install

The full end-to-end guide — store source, installation, credentials, and first request — is
in the [repository README](../README.md#install-auth-cursor).

This document covers configuration and behaviour after the plugin is installed.

## Configure

[config.example.yaml](config.example.yaml) is a complete host configuration you can copy
from. The block that matters is:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    auth-cursor:
      enabled: true
      # Leave empty to download the pinned cursor-sdk-bridge on first use.
      bridge-path: ""
      # Proxy for credentials whose auth file sets no proxy_url.
      proxy-url: ""
      # Cursor Router mode applied to the auto-smart model: cost | balanced | intelligence.
      optimize-for: "balanced"
      # Per-credential share for weighted-round-robin routing. See "Load balancing".
      weights: {}
```

| Option | Purpose |
| --- | --- |
| `bridge-path` | The `cursor-sdk-bridge` executable, or the directory holding it. Empty means the automatic download. |
| `proxy-url` | Proxy for credentials whose auth file has no `proxy_url`. See [Proxying](#proxying). |
| `optimize-for` | Router mode for the `auto-smart` model: `cost`, `balanced`, or `intelligence`. |
| `weights` | Per-credential share for weighted routing, keyed by account email or auth file name. |

When the host runs under a service manager, prefer an absolute path for `plugins.dir`: the
working directory of a launchd or systemd unit differs from an interactive shell.

### The bridge binary

The plugin looks for the bridge in this order, most explicit first:

1. `CURSOR_SDK_BRIDGE_BIN`, an absolute path to the executable;
2. `bridge-path` from the config, either the executable or the directory holding it;
3. the copy already unpacked at
   `<cache>/cli-proxy-api/auth-cursor-bridge/<bridge-version>/bin/cursor-sdk-bridge`
   (`~/Library/Caches` on macOS, `~/.cache` or `$XDG_CACHE_HOME` on Linux, `%LocalAppData%`
   on Windows);
4. a download of the pinned release from GitHub.

The download is verified against a SHA-256 that ships inside the plugin, not against the
checksum file published beside the archive: a checksum served from the same place as the
archive it describes proves only that the two agree. A mismatch aborts the install and
leaves nothing behind.

Pinned to one bridge version deliberately — the `sdk.v1` contract the plugin is generated
against comes from that same release. If GitHub is unreachable from the host, download
`cursor-sdk-bridge-standalone-<platform>.tar.gz` from
[cursor/sdk-bridge releases](https://github.com/cursor/sdk-bridge/releases) elsewhere,
unpack it, and point `bridge-path` at it.

One bridge process is started per proxy, shared by every credential that uses it, and each
gets a private empty workspace directory. Durable agent state lives under
`<cache>/cli-proxy-api/auth-cursor-bridge/<plugin-version>/state/`; the plugin deletes each
one-shot agent after its request, so that directory does not grow with traffic.

## Credentials

A Cursor credential is an API key you create yourself: a personal key in the
[Cursor dashboard](https://cursor.com/dashboard), or a team service-account key. There is no
browser sign-in — `sdk.v1` exposes no login endpoint, and earlier versions of this plugin
minted keys through a flow that is no longer available to it.

### Import from the command line

The plugin registers `--cursor-login` on the host binary. It prompts for the key, checks it,
and writes the auth file:

```bash
./cli-proxy-api --cursor-login
Paste your Cursor API key (https://cursor.com/dashboard): key_...
```

The key is checked against Cursor before anything is saved, so a mistyped key fails here
instead of at the first request. The account email that check returns names the file —
`cursor-<account>.json` in `auth-dir` — so importing a key for the same account replaces
that file rather than adding a duplicate. The command exits without starting the server.

For scripts and CI, pass the key as an argument instead:

```bash
./cli-proxy-api --cursor-login --cursor-api-key "key_..."
```

Prefer the prompt when a person is present: an argument is visible in the process list and
in shell history. The import never reads the key from the environment, so nothing is picked
up from an ambient `CURSOR_API_KEY`.

Under a service manager, or with stdin redirected, there is no terminal to prompt on and the
command says so — use `--cursor-api-key` there.

Re-importing does not cost you the settings you added to that file. The plugin reads the file
it is about to replace and carries over everything it does not own — `label`, `prefix`,
`proxy_url`, `disabled`, `note`, `model_aliases`, `excluded-models` and any custom field —
while the key itself is replaced.

### Writing the auth file yourself

The import is a convenience. An auth file in `auth-dir`, for example
`auths/cursor-main.json`, works exactly the same:

```json
{
  "type": "cursor",
  "api_key": "key_..."
}
```

`type` is the provider key `cursor`, not the plugin ID `auth-cursor`.

Optional fields: `label`, `prefix`, `proxy_url`, `disabled`, `note`, and the per-account
`model_aliases` / `excluded-models` lists described under [Models](#models).

Dashboard keys carry no expiry the plugin can read, so they are re-checked only for
presence. A key revoked in the dashboard starts failing with 401 at request time. Auth files
written by the old login flow may carry `expires_at`; it is still honoured, and re-importing
clears it.

There is no `cursor-api-key` config field. A key is a credential, and credentials live in
`auth-dir` where the host can schedule several of them.

### Load balancing

Add one file per key to spread traffic across credentials; the host schedules them like any
other provider. To give some accounts a larger share, switch the host to the weighted
strategy and list the shares in the plugin config:

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

Each key is matched case-insensitively against the credential's `email` first, then the auth
file name with and without the `.json` suffix. A credential you do not list keeps the host
default share of 1. A share of `0` — or any negative value — parks the credential while the
weighted strategy is active. The maximum is 1000000, and a value the host would reject fails
the config load rather than surfacing later as a routing error.

Weights are configured here rather than in the auth file for a reason: `--cursor-login`
rewrites that file. Do not set `weight` in the auth file as well. The host lets a file-level
`weight` override whatever the plugin resolved, so a leftover one silently wins and then
disappears on the next import.

Editing `weights` takes effect through the host's normal config reload; no restart is needed.

## Proxying

Cursor traffic does not pass through the host's HTTP client any more: the bridge opens its
own HTTPS connections. The host-level `proxy-url` therefore has no effect on it, and the
proxy has to be given to the bridge instead.

The plugin resolves one per credential — the auth file's `proxy_url` first, then the plugin's
`proxy-url` — and starts one bridge process per resolved proxy, passing it in the process
environment (`HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and their lowercase spellings). A
proxy already present in the host's own environment is left alone when the plugin has none of
its own, so a machine that only reaches the internet through a proxy keeps working.

Image attachments are the exception: a remote image URL is fetched through the host's HTTP
client and sent to Cursor as inline bytes, because `sdk.v1` accepts image references only for
cloud-routed agents.

## Models

Model availability is per account and per team, so the plugin asks Cursor for the catalog
with each credential instead of hard-coding a list. Whatever that key can reach shows up on
`/v1/models`: Composer, Grok, and the OpenAI, Anthropic and Google models in the Cursor
usage pool. Cursor Router appears as `auto-smart` when the team has it enabled.

`optimize-for` is applied automatically to `auto-smart`, which rejects requests that omit it.
Other per-model parameters are checked against the catalog and dropped when the selected
model does not expose them, so a `reasoning_effort` sent to a model that has no such
parameter is ignored rather than rejected upstream.

Models are matched by id only. The TypeScript SDK's catalog also carried an alias list per
model, which the previous implementation accepted as request ids; `sdk.v1` has no such field,
so a second name for a model now has to come from the host's `oauth-model-alias` block
below.

Discovery is the only source of the catalog; there is no model list to configure. When it
fails for a credential the plugin publishes nothing for that key, so `/v1/models` carries no
Cursor entries until a later discovery succeeds. A catalog that cannot be read does not block
generation: the requested model id is forwarded as-is and Cursor decides.

### Renaming and hiding models

Cursor credentials are OAuth-kind credentials as far as the host is concerned, so the
host-level alias and exclusion blocks apply to them. Both are keyed by the provider key
`cursor`, not by the plugin ID, and both live outside `plugins.configs`:

```yaml
oauth-model-alias:
  cursor:
    - name: "grok-4.6"      # upstream id reported by Cursor
      alias: "grok-latest"  # id your clients use
      # fork: true          # also keep the upstream id on /v1/models
      # display-name: "Grok Latest"

oauth-excluded-models:
  cursor:
    - "grok-4.5"
```

These apply to every Cursor credential. To scope a rename or an exclusion to one account,
put `model_aliases` / `excluded-models` in that account's auth file instead; the per-account
list is consulted first and wins over the global block.

## Behaviour and limits

- **Agent semantics, not raw inference.** Even when you select a Claude or GPT model, the
  request runs through the Cursor agent harness and bills against the Cursor usage pool.
  It is not a direct Anthropic or OpenAI API call.
- **Stateless.** Every request builds a fresh agent from the full message list; conversation
  state is not retained between requests.
- **Latency.** Agent creation adds overhead, so time-to-first-token is higher than a native
  inference API.
- **A cancelled request cancels the run.** Dropping the bridge stream does not stop a run —
  Cursor keeps executing and billing it — so an abandoned request explicitly cancels the run
  the stream reported, then deletes the agent.
- **No token counting endpoint.** `count_tokens` returns a character-based estimate; billing
  uses the counts Cursor reports after a run.
- **No raw HTTP passthrough.** `executor.http_request` returns 501.
- **Upstream failures are classified before the host sees them.** A rejected key, a rate
  limit and a server error each get the handling they deserve. Refusals that are properties
  of the request — a model the account's region or plan cannot reach — are reported as
  request faults, so they fail that one request without parking the credential for every
  other model.
- **Stream framing depends on the caller.** Chat-completions clients receive chunks that the
  host frames itself, while other protocols go through a response translator that requires
  `data:` frames. The executor picks the framing from the inbound request path; without it,
  one of the two paths gets malformed SSE.

## Usage and compliance

The plugin only uses Cursor's published SDK Bridge contract and keys you create yourself, so
it does not touch private endpoints or credentials belonging to the Cursor editor. How you
operate it still matters, because every request bills a real Cursor account:

- **Do not share one account's key with other people.** Exposing this proxy to others on
  your own credential is account sharing, which Cursor's terms prohibit. Give each person
  their own key, or use a team service-account key.
- **Do not resell or publicly redistribute Cursor access** obtained through this plugin.
- **Expect rate limiting under automated load.** Rotating several credentials spreads usage,
  but each one still draws on a real account's quota.

Review Cursor's current terms before deploying; this document is not legal advice.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Plugin missing from `/v0/management/plugins` | `plugins.enabled` is off, or `plugins.dir` does not resolve — use an absolute path under a service manager |
| `"registered": false` | The library failed to load; the host binary may lack CGO plugin support |
| `create plugin directory: mkdir plugins: read-only file system` (or `mkdir ~: …`) | Relative `plugins.dir` or a literal `~` is resolved against the Homebrew/launchd CWD (`/`). Use `/Users/<you>/.cli-proxy-api/plugins`. See [CLIProxyAPI #4313](https://github.com/router-for-me/CLIProxyAPI/issues/4313). |
| `download …/cursor-sdk-bridge-standalone-…tar.gz` fails | The host cannot reach GitHub releases. Download the archive elsewhere, unpack it, and set `bridge-path`. |
| `verify …: sha256 mismatch` | The downloaded archive is not the pinned release — a proxy or mirror rewrote it. Nothing was installed; fix the network path or set `bridge-path`. |
| `cursor sdk bridge was not ready within 30s` | The bridge started but never announced its port. The log line `cursor sdk bridge stderr` carries its own diagnostics. |
| `cursor sdk bridge exited before it was ready` | The binary could not run at all — wrong platform archive, or a `bridge-path` pointing at something else. |
| `/v1/models` has no Cursor entries | Discovery failed for that credential; check the `cursor model discovery failed` log line for the reason |
| `cursor upstream error 401: Invalid User API Key` | The key is rejected or revoked; the host then parks the credential, so later requests report `auth_unavailable` instead of repeating the 401 |
| `Model not available … not supported in your region` | The account's region or plan cannot reach that model — typically the Claude and GPT entries. Only the requested model is refused; the credential keeps serving the rest. Hide the unreachable ids with `oauth-excluded-models.cursor`, or per account with `excluded-models` in the auth file. |
| Every Cursor request reports `503 auth_unavailable` after one model failed | An unclassified upstream failure parks the credential for a cooldown window. Check `~/.cli-proxy-api/logs/main.log` for the `upstream execution failed` line naming the real cause; `disable-cooling: true` is an emergency escape while you fix it. |
| Hundreds of `auth-cursor-sidecar/.../package.json` credentials in the management panel | Version 1.0.0 and earlier bootstrapped a Node sidecar into `~/.cli-proxy-api`, which is the default `auth-dir`, so every npm manifest was scanned as a credential. This version runs no sidecar; delete `~/.cli-proxy-api/auth-cursor-sidecar/` by hand. |

## Development

Building from source: see the [repository README](../README.md#building-from-source).

```bash
make build          # dist/auth-cursor.<ext>
make fmt
make test
make proto          # only when the pinned bridge release changes
```

The tests drive the plugin against an in-process fake `sdk.v1` service, so they need no
Cursor credential, no bridge binary and no network access. They cover the startup handshake
including its timeout and failure paths, discovery validation, process pooling per proxy,
the bearer token the bridge requires, bridge download verification and archive extraction
safety, run streaming and keepalive handling, run cancellation, agent teardown, error-code
classification, model parameter filtering and `optimize_for` backfill, image inlining, prompt
flattening, completion and stream chunk assembly, per-protocol stream framing, auth file
claiming, weight resolution, and the `--cursor-login` import including the settings it carries
over.

The startup contract itself — the ready line, the discovery payload, the bearer token — can be
checked against the real binary, which needs no Cursor credential:

```bash
cd go && CURSOR_SDK_BRIDGE_BIN=/path/to/cursor-sdk-bridge go test -run TestLiveBridgeHandshake ./...
```

`sdk.v1` is vendored under [proto/](proto) exactly as the pinned bridge release publishes it,
and the generated clients under `go/internal/` are committed, so building needs nothing but
the Go toolchain. Regenerating needs [buf](https://buf.build) with `protoc-gen-go` and
`protoc-gen-connect-go` on PATH.

Bumping the bridge: replace `proto/`, update `bridgeVersion` and the checksums in
`go/bridge_release.go`, run `make proto`, and check the generated diff. The three move
together — the contract the plugin is generated against belongs to the release it drives.
