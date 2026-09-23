# auth-commandcode

Exposes Command Code models on CLIProxyAPI's OpenAI, Claude, and Gemini compatible endpoints through the official [Provider API](https://commandcode.ai/docs/provider).

Two names, not interchangeable:

| | Value | Used for |
| --- | --- | --- |
| Plugin ID | `auth-commandcode` | Store, `plugins.configs`, library filename |
| Provider key | `commandcode` | Auth files, `--commandcode-login`, `owned_by` on `/v1/models`, `oauth-model-alias` / `oauth-excluded-models` |

This plugin talks HTTP. There is no sidecar, SDK bridge, or Node.js runtime.

```
client (OpenAI / Claude / Gemini)
  -> host translates to chat-completions
    -> auth-commandcode (C ABI)
      -> https://api.commandcode.ai/provider/v1/chat/completions
         or /provider/v1/messages for Claude models
```

A full host config is in [config.example.yaml](config.example.yaml).

## Use

Store source and `plugins.dir` are in the [repository README](../README.md). This section is what this plugin adds on top.

You need a Command Code API key from [Studio](https://commandcode.ai/studio). The same key authenticates the CLI and the Provider API. The Go plan has no API access (`403 upgrade_required`).

```yaml
plugins:
  configs:
    auth-commandcode:
      enabled: true
      api-base: "https://api.commandcode.ai"
      proxy-url: ""
      zdr: false
      weights: {}
```

Restart, install, and check that it actually registered:

```bash
curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  localhost:8317/v0/management/plugin-store

curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  "localhost:8317/v0/management/plugin-store/auth-commandcode/install"

curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  localhost:8317/v0/management/plugins
```

Expect `"registered": true` and `"effective_enabled": true`. The store writes a **versioned** filename, for example `plugins/darwin/arm64/auth-commandcode-v1.0.0.dylib`.

Import a key and send a request:

```bash
./cli-proxy-api --commandcode-login
# no TTY (brew services, redirect):
# ./cli-proxy-api --commandcode-login --commandcode-api-key "user_..."

./cli-proxy-api &
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/models
curl -H "Authorization: Bearer $API_KEY" localhost:8317/v1/chat/completions \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

## Configure

`plugins.configs.auth-commandcode`:

| Option | Purpose |
| --- | --- |
| `api-base` | Command Code origin. Default `https://api.commandcode.ai`. Provider paths are appended |
| `proxy-url` | Proxy used when the auth file has no `proxy_url`. Otherwise `host.http` uses the host-level proxy |
| `zdr` | Send `x-cmd-zdr: 1` unless the client already set that header |
| `weights` | Per-credential share for weighted routing, keyed by auth file name |

Model aliases and exclusions are **host-level**, keyed by `commandcode`, not inside `plugins.configs`.

### Credentials

There is no browser sign-in in this plugin. Create the key in Studio, then import it.

```bash
./cli-proxy-api --commandcode-login
./cli-proxy-api --commandcode-login --commandcode-api-key "user_..."
# unreachable API, still persist the file:
# ./cli-proxy-api --commandcode-login --commandcode-api-key "user_..." --commandcode-skip-validate
```

The key is checked against the documented `GET /provider/v1/models` endpoint before anything is saved, unless `--commandcode-skip-validate` is set. The import does **not** read `COMMAND_CODE_API_KEY` and does **not** import `~/.commandcode/auth.json`. Those are Command Code CLI mechanisms; this plugin only uses keys you pass on the host.

The file name is `commandcode-<digest>.json`, where the digest is a short SHA-256 of the key. Importing the same key again replaces that credential and keeps operator-owned fields (`label`, `proxy_url`, `disabled`, …).

Or write the file yourself. `type` is `commandcode`, not `auth-commandcode`:

```json
{
  "type": "commandcode",
  "api_key": "user_..."
}
```

Optional fields: `label`, `prefix`, `proxy_url`, `disabled`, `note`, `model_aliases`, `excluded-models`.

Studio keys have no expiry the plugin can read. A key revoked in Studio starts failing with 401 at request time.

### Models

Availability is per account. The plugin asks `GET /provider/v1/models` with each credential. Claude ids (`claude-…`, case-insensitive) are sent to `/provider/v1/messages`; every other id is sent to `/provider/v1/chat/completions`. The plugin does not retry the other endpoint after a 400, so a mis-routed request is not billed twice.

Aliases and exclusions are host-level, keyed by `commandcode`:

```yaml
oauth-model-alias:
  commandcode:
    - name: "deepseek/deepseek-v4-flash"
      alias: "deepseek-flash"

oauth-excluded-models:
  commandcode:
    - "claude-*"
```

### Errors

| Upstream | Plugin / host |
| --- | --- |
| `401` | Credential problem |
| `400`, `403 upgrade_required`, `422 cmd_zdr_no_providers` | This request failed; the key stays in rotation |
| `429`, `5xx` | Retryable |

`count_tokens` is a character estimate. `executor.http_request` returns 501. Anthropic `max_tokens` defaults to 8192 when the client omits it; zero or values above 200000 are rejected locally with 400.

### Proxying

When an auth file or `plugins.configs.auth-commandcode.proxy-url` is set, that proxy is used. Otherwise upstream calls go through `host.http.do` / `do_stream`, so the host-level `proxy-url` applies and request-log can record the HTTP hop.

Remote images on Claude requests are fetched by the plugin, not forwarded as URLs. Loopback, private, link-local, CGNAT, metadata hosts, URL credentials, and redirects onto those destinations are refused. The error text never repeats the path or query.

## Develop

Needs a CGO-capable Go toolchain.

```bash
cd auth-commandcode
make fmt
make test
make build                        # dist/auth-commandcode.<ext>
```

Tests use `httptest`. They do not call Command Code, write a user auth-dir, or use a real key.

### Load a local build

```bash
make build
cp dist/auth-commandcode.dylib ~/.cli-proxy-api/plugins/darwin/arm64/auth-commandcode.dylib
# if the host still records a store install, also overwrite the versioned name
chmod +x ~/.cli-proxy-api/plugins/darwin/arm64/auth-commandcode*.dylib
```

A `make build` without `-ldflags` reports `0.0.0-dev`.
