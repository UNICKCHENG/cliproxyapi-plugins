// NDJSON stdio bridge between the CLIProxyAPI cursor plugin and the Cursor Agent SDK.
//
// Protocol: one JSON object per line on stdin, zero or more JSON events per line on stdout.
// Requests are multiplexed by "id" and handled concurrently, so the plugin can keep a single
// sidecar process for every credential and every in-flight request.
//
// Request  : {id, op: "models"|"generate"|"login"|"cancel", api_key, model, params, prompt, images, stream}
// Response : {id, event: "models"|"delta"|"done"|"login_url"|"login"|"error", ...}
// HTTP     : {id, event: "http", ...} bridged through host.http.* in the Go plugin

import { createHash } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { dispatchHostHttpReply, installHostFetchBridge } from "./http-bridge.mjs";

installHostFetchBridge();

let sdk;

// The SDK is loaded lazily so that a misconfigured install surfaces as a structured error
// event on the first request instead of an unexplained process exit at startup.
async function loadSdk() {
  if (!sdk) {
    const loaded = await import("@cursor/sdk");
    loaded.configureCursorSdk({ local: { useHttp1ForAgent: true } });
    sdk = loaded;
  }
  return sdk;
}

// Requests carry their own credential, so a single sidecar serves every configured auth.
// The catalog is cached per credential to keep parameter validation off the hot path.
const catalogCache = new Map();
const activeRuns = new Map();
const activeLogins = new Map();

// Runs execute against a throwaway empty directory: the agent is used purely as a text
// generator, so it must not see the host's working tree.
const workspace = mkdtempSync(join(tmpdir(), "cliproxy-cursor-"));

function cacheKey(apiKey) {
  return createHash("sha256").update(apiKey ?? "").digest("hex");
}

function send(payload) {
  process.stdout.write(`${JSON.stringify(payload)}\n`);
}

// "status" is reserved for the run status string on done events, so the HTTP status of a
// failure travels as http_status to keep every field single-typed across event kinds.
function sendError(id, err) {
  send({
    id,
    event: "error",
    message: err?.message ? String(err.message) : String(err),
    code: err?.code ? String(err.code) : "",
    http_status: Number.isInteger(err?.status) ? err.status : 0,
    retryable: err?.isRetryable === true,
  });
}

async function listModels(apiKey) {
  const key = cacheKey(apiKey);
  if (catalogCache.has(key)) {
    return catalogCache.get(key);
  }
  const { Cursor } = await loadSdk();
  const models = await Cursor.models.list({ apiKey });
  const normalized = Array.isArray(models) ? models : [];
  catalogCache.set(key, normalized);
  return normalized;
}

function findCatalogEntry(catalog, modelId) {
  if (!modelId) {
    return undefined;
  }
  return catalog.find(
    (entry) => entry.id === modelId || (entry.aliases ?? []).includes(modelId),
  );
}

// Builds a ModelSelection that the SDK will accept: unknown parameters are dropped rather
// than rejected upstream, and the router model always carries an explicit optimize_for.
function resolveModelSelection(catalog, modelId, requestedParams, optimizeFor) {
  const requested = Array.isArray(requestedParams) ? requestedParams : [];
  const entry = findCatalogEntry(catalog, modelId);
  if (!entry) {
    // Unknown to the catalog (or catalog unavailable): forward as-is and let the SDK decide.
    return requested.length > 0 ? { id: modelId, params: requested } : { id: modelId };
  }

  const definitions = entry.parameters ?? [];
  const params = [];
  for (const definition of definitions) {
    const allowed = (definition.values ?? []).map((item) => String(item.value));
    const supplied = requested.find((param) => param.id === definition.id);
    if (supplied && allowed.includes(String(supplied.value))) {
      params.push({ id: definition.id, value: String(supplied.value) });
      continue;
    }
    // Router rejects requests that omit optimize_for, so fall back to the configured mode.
    if (definition.id === "optimize_for") {
      const preferred = allowed.includes(optimizeFor) ? optimizeFor : allowed[0];
      if (preferred) {
        params.push({ id: definition.id, value: preferred });
      }
    }
  }
  return params.length > 0 ? { id: entry.id, params } : { id: entry.id };
}

async function handleModels(request) {
  const catalog = await listModels(request.api_key);
  send({
    id: request.id,
    event: "models",
    models: catalog.map((entry) => ({
      id: entry.id,
      display_name: entry.displayName ?? entry.id,
      description: entry.description ?? "",
      aliases: entry.aliases ?? [],
      parameters: (entry.parameters ?? []).map((parameter) => parameter.id),
    })),
  });
}

async function handleGenerate(request) {
  const { Agent } = await loadSdk();

  let catalog = [];
  try {
    catalog = await listModels(request.api_key);
  } catch {
    // Discovery is advisory. A catalog failure must not block generation.
  }
  const model = resolveModelSelection(
    catalog,
    request.model,
    request.params,
    request.optimize_for || "balanced",
  );

  const agent = await Agent.create({
    apiKey: request.api_key,
    model,
    // No built-in tools: the agent may only answer with text, which is what a model
    // proxy request means. Shell, edit and search tools would be meaningless here.
    tools: [],
    local: { cwd: workspace },
  });

  try {
    const message =
      Array.isArray(request.images) && request.images.length > 0
        ? { text: request.prompt, images: request.images }
        : request.prompt;

    const streaming = request.stream === true;
    const run = await agent.send(
      message,
      streaming
        ? {
            onDelta: ({ update }) => {
              if (update.type === "text-delta" && update.text) {
                send({ id: request.id, event: "delta", text: update.text });
              }
            },
          }
        : undefined,
    );
    activeRuns.set(request.id, run);

    const result = await run.wait();
    if (result.status === "error") {
      throw Object.assign(new Error(result.error?.message ?? "cursor run failed"), {
        code: result.error?.code,
      });
    }
    send({
      id: request.id,
      event: "done",
      status: result.status,
      text: result.result ?? "",
      usage: result.usage ?? null,
    });
  } finally {
    activeRuns.delete(request.id);
    agent.close();
  }
}

// handleLogin mints a user API key through the browser sign-in flow. Persistence is left to
// the host (store: null) so the key lands in the configured auth directory with every other
// credential instead of in ~/.cursor/sdk/auth.json.
async function handleLogin(request) {
  const { Cursor } = await loadSdk();
  const controller = new AbortController();
  activeLogins.set(request.id, controller);
  try {
    const result = await Cursor.auth.login({
      store: null,
      openBrowser: request.no_browser !== true,
      apiKeyName: request.api_key_name || "CLIProxyAPI",
      onLoginUrl: (url) => send({ id: request.id, event: "login_url", url: String(url) }),
      signal: controller.signal,
    });
    send({
      id: request.id,
      event: "login",
      api_key: result.apiKey,
      email: result.email ?? "",
      expires_at_ms: Number.isFinite(result.apiKeyExpiresAtMs) ? result.apiKeyExpiresAtMs : 0,
    });
  } finally {
    activeLogins.delete(request.id);
  }
}

async function handleCancel(request) {
  const run = activeRuns.get(request.id);
  if (run) {
    await run.cancel().catch(() => {});
  }
  const login = activeLogins.get(request.id);
  if (login) {
    login.abort();
  }
}

async function dispatch(request) {
  switch (request.op) {
    case "models":
      return handleModels(request);
    case "generate":
      return handleGenerate(request);
    case "login":
      return handleLogin(request);
    case "cancel":
      return handleCancel(request);
    default:
      throw new Error(`unknown op: ${request.op}`);
  }
}

let inFlight = 0;
let stdinEnded = false;

// Closing stdin means the plugin is shutting down, but requests already accepted still owe
// the caller an event, so the process only exits once they have all settled.
function exitWhenDrained() {
  if (stdinEnded && inFlight === 0) {
    cleanup();
    process.exit(0);
  }
}

function handleLine(line) {
  const trimmed = line.trim();
  if (trimmed === "") {
    return;
  }
  let request;
  try {
    request = JSON.parse(trimmed);
  } catch (err) {
    sendError("", err);
    return;
  }
  // Not awaited: requests are multiplexed by id and must not block the read loop.
  inFlight += 1;
  dispatch(request)
    .catch((err) => sendError(request.id, err))
    .finally(() => {
      inFlight -= 1;
      exitWhenDrained();
    });
}

let pending = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  pending += chunk;
  let newline = pending.indexOf("\n");
  while (newline >= 0) {
    const line = pending.slice(0, newline);
    pending = pending.slice(newline + 1);
    if (!dispatchHostHttpReply(line)) {
      handleLine(line);
    }
    newline = pending.indexOf("\n");
  }
});
process.stdin.on("end", () => {
  stdinEnded = true;
  exitWhenDrained();
});

function cleanup() {
  try {
    rmSync(workspace, { recursive: true, force: true });
  } catch {
    // Best effort: the OS reclaims the temp directory anyway.
  }
}

process.on("SIGTERM", () => {
  cleanup();
  process.exit(0);
});
process.on("SIGINT", () => {
  cleanup();
  process.exit(0);
});
