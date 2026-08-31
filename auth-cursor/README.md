# auth-cursor

Exposes Cursor models through CLIProxyAPI's OpenAI, Claude and Gemini compatible endpoints.

Two names matter here and they are not the same:

- the **plugin ID** is `auth-cursor` — used for the store API and the `plugins.configs` key;
- the **provider key** is `cursor` — used by credentials (`"type": "cursor"`), the
  `--cursor-login` flag, and `owned_by` on `/v1/models`.

`@cursor/sdk` is a Node-only *agent* SDK, not a chat-completions API, and there is no Go
client for it. The plugin therefore ships two pieces:

- a C-ABI shared library (`auth-cursor.dylib` / `.so` / `.dll`) that the host loads and that
  declares the `executor`, `auth_provider`, `model_provider` and `command_line_plugin`
  capabilities;
- a long-lived Node sidecar that owns the SDK and speaks NDJSON over stdio.

Each request creates a one-shot agent with an empty working directory and `tools: []`, so
the agent can only answer with text. That reduces an agent run to plain model inference,
which is what a proxy request means.

```
client (OpenAI/Claude/Gemini)
  -> host translates to chat-completions
    -> auth-cursor plugin (C ABI)
      -> Node sidecar (@cursor/sdk)
        -> host.http.* (proxy, logging, transport policy)
          -> Cursor backend
```

## Install

The full end-to-end guide — prerequisites (including Node.js and npm), store source,
installation, login, and first request — is in the
[repository README](../README.md#install-auth-cursor).

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
      # Node executable used to run the sidecar. Defaults to "node" from PATH.
      # Under launchd or systemd, set an absolute path — see the install guide.
      node-path: "node"
      # Leave empty: the plugin auto-discovers a local auth-cursor-sidecar directory,
      # then falls back to ~/.cli-proxy-api/auth-cursor-sidecar/<version>/.
      sidecar-path: ""
      # Cursor Router mode applied to the auto-smart model: cost | balanced | intelligence.
      optimize-for: "balanced"
      # Per-credential share for weighted-round-robin routing. See "Load balancing".
      weights: {}
```

| Option | Purpose |
| --- | --- |
| `node-path` | Node binary that runs the sidecar. Required at runtime; not bundled by the store install. |
| `sidecar-path` | Override sidecar location. Leave empty for automatic bootstrap. |
| `optimize-for` | Router mode for the `auto-smart` model: `cost`, `balanced`, or `intelligence`. |
| `weights` | Per-credential share for weighted routing, keyed by account email or auth file name. |

When the host runs under a service manager, prefer absolute paths for `plugins.dir` and
`node-path`: the working directory and PATH of a launchd or systemd unit differ from an
interactive shell.

## Credentials

### Login from the command line

The plugin registers a `--cursor-login` flag on the host binary:

```bash
./cli-proxy-api --cursor-login              # opens the browser
./cli-proxy-api --cursor-login --no-browser # prints the URL instead
```

The sign-in URL is printed as soon as Cursor reports it. Completing the login mints a user
API key named `CLIProxyAPI`, and the host saves it into the configured `auth-dir` as
`cursor-<account>.json`, so logging in again with the same account replaces that file
instead of adding a duplicate. The command exits without starting the server.

Keys minted this way expire (90 days by default) and Cursor offers no refresh token, so the
plugin records the expiry in the auth file and schedules the host to re-check it then. Once
past that point the credential is reported invalid and `--cursor-login` must be run again.

Renewing does not cost you the settings you added to that file. The plugin reads the file it
is about to replace and carries over everything it does not own — `label`, `prefix`,
`proxy_url`, `disabled`, `note`, `model_aliases`, `excluded-models` and any custom field —
while the freshly minted key and its expiry always replace the previous ones.

Login runs on the machine that owns the browser, which is why there is no equivalent flow in
the management panel or TUI.

### Using a dashboard key

A key created in the Cursor dashboard works as well: save it as an auth file in `auth-dir`,
for example `auths/cursor-main.json`:

```json
{
  "type": "cursor",
  "api_key": "key_..."
}
```

`type` is the provider key `cursor`, not the plugin ID `auth-cursor`.

Optional fields: `label`, `prefix`, `proxy_url`, `disabled`, `note`, and the per-account
`model_aliases` / `excluded-models` lists described under [Models](#models). Dashboard keys
carry no expiry the plugin can read, so they are re-checked only for presence.

Both sources produce the same thing — one Cursor user API key in one auth file. There is no
separate `cursor-api-key` config block, and nothing else to set for a key to be picked up.

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
disappears on the next login.

Editing `weights` takes effect through the host's normal config reload; no restart is needed.

## Models

Model availability is per account and per team, so the plugin calls `Cursor.models.list()`
with each credential instead of hard-coding a list. Whatever that key can reach shows up on
`/v1/models`: Composer, Grok, and the OpenAI, Anthropic and Google models in the Cursor
usage pool. Cursor Router appears as `auto-smart` when the team has it enabled.

`optimize-for` is applied automatically to `auto-smart`, which rejects requests that omit it.
Other per-model parameters are validated against the catalog and dropped when unsupported,
so a `reasoning_effort` sent to a model that has no such parameter is ignored rather than
rejected upstream.

Discovery is the only source of the catalog; there is no model list to configure. When it
fails for a credential the plugin publishes nothing for that key, so `/v1/models` carries no
Cursor entries until a later discovery succeeds.

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
- **Proxying.** Upstream HTTPS from `@cursor/sdk` is routed back through the host with
  `host.http.do` / `host.http.do_stream`, so `proxy-url` and the host transport policy apply
  without any extra environment variables. The sidecar patches `globalThis.fetch` before the
  SDK loads and forces HTTP/1.1 agent streams for proxy compatibility.
- **No token counting endpoint.** `count_tokens` returns a character-based estimate; billing
  uses the counts Cursor reports after a run.
- **No raw HTTP passthrough.** `executor.http_request` returns 501.
- **Stream framing depends on the caller.** Chat-completions clients receive chunks that the
  host frames itself, while other protocols go through a response translator that requires
  `data:` frames. The executor picks the framing from the inbound request path; without it,
  one of the two paths gets malformed SSE.

## Usage and compliance

The plugin only uses documented `@cursor/sdk` entry points and the browser sign-in flow, so
it does not touch private endpoints or credentials belonging to the Cursor editor. How you
operate it still matters, because every request bills a real Cursor account:

- **Do not share one account's key with other people.** Exposing this proxy to others on
  your own credential is account sharing, which Cursor's terms prohibit. Give each person
  their own `--cursor-login` credential, or use a team service-account key.
- **Do not resell or publicly redistribute Cursor access** obtained through this plugin.
- **Expect rate limiting under automated load.** Rotating several credentials spreads usage,
  but each one still draws on a real account's quota.

Review Cursor's current terms before deploying; this document is not legal advice.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Plugin missing from `/v0/management/plugins` | `plugins.enabled` is off, or `plugins.dir` does not resolve — use an absolute path under a service manager |
| `"registered": false` | The library failed to load; the host binary may lack CGO plugin support |
| `/v1/models` has no Cursor entries | Discovery failed for that credential, usually because the sidecar cannot start |
| `no such file or directory` naming `node` | `node-path` is not resolvable from the host's PATH; set an absolute path (see the [install guide](../README.md#install-auth-cursor)) |
| `cursor upstream error 401: Invalid User API Key` | The key is rejected; the host then parks the credential, so later requests report `auth_unavailable` instead of repeating the 401 |
| First request hangs for a minute | The one-time sidecar bootstrap is running `npm install`; check the logs for `installing cursor sidecar dependencies` |

## Development

Building from source: see the [repository README](../README.md#building-from-source).

```bash
make build          # dist/auth-cursor.<ext>
make sidecar-deps   # go/sidecar/node_modules for local runs
make fmt
make test
```

The tests drive the executor against a fake NDJSON sidecar, so they need `node` on PATH but
no Cursor credential and no network access. They cover prompt flattening, image and
parameter mapping, completion and stream chunk assembly, per-protocol stream framing, the
host HTTP bridge encoding, sidecar bootstrap resolution, auth file claiming, credential
expiry, weight resolution, the `--cursor-login` command including the settings it carries
over, and upstream error propagation.

The full bootstrap, including the real `npm install`, is covered by an opt-in test:

```bash
cd go && CURSOR_PLUGIN_BOOTSTRAP_TEST=1 go test -run TestBootstrapSidecarScript ./...
```
