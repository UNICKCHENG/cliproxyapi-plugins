// Routes @cursor/sdk fetch calls through the CLIProxyAPI host via NDJSON stdio.
//
// Sidecar -> plugin (stdout): {id, event:"http", method, url, headers, body, stream}
// Plugin -> sidecar (stdin):  {id, op:"http_response"|"http_headers"|"http_chunk"|"http_end"|"http_error", ...}

const pending = new Map();
let seq = 0;

function emit(payload) {
  process.stdout.write(`${JSON.stringify(payload)}\n`);
}

function headersToRecord(headers) {
  const out = {};
  if (!headers) {
    return out;
  }
  if (headers instanceof Headers) {
    headers.forEach((value, key) => {
      if (!out[key]) {
        out[key] = [];
      }
      out[key].push(value);
    });
    return out;
  }
  for (const [key, value] of Object.entries(headers)) {
    if (Array.isArray(value)) {
      out[key] = value.map(String);
    } else if (value != null) {
      out[key] = [String(value)];
    }
  }
  return out;
}

function recordToHeaders(record) {
  const headers = new Headers();
  if (!record) {
    return headers;
  }
  for (const [key, values] of Object.entries(record)) {
    for (const value of values ?? []) {
      headers.append(key, value);
    }
  }
  return headers;
}

async function readBody(input, init) {
  if (init.body != null) {
    if (typeof init.body === "string") {
      return init.body;
    }
    if (init.body instanceof URLSearchParams) {
      return init.body.toString();
    }
    if (init.body instanceof ArrayBuffer) {
      return Buffer.from(init.body);
    }
    if (ArrayBuffer.isView(init.body)) {
      return Buffer.from(init.body.buffer, init.body.byteOffset, init.body.byteLength);
    }
    if (typeof init.body === "object" && Symbol.asyncIterator in init.body) {
      const chunks = [];
      for await (const chunk of init.body) {
        chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
      }
      return Buffer.concat(chunks);
    }
  }
  if (input instanceof Request && !input.bodyUsed) {
    const buffer = Buffer.from(await input.arrayBuffer());
    return buffer.length > 0 ? buffer : undefined;
  }
  return undefined;
}

function resolveRequestURL(input) {
  if (typeof input === "string") {
    return new URL(input);
  }
  if (input instanceof URL) {
    return new URL(input.href);
  }
  if (input instanceof Request) {
    return new URL(input.url);
  }
  return null;
}

function mergeInit(input, init) {
  const merged = { ...init };
  if (input instanceof Request) {
    merged.method = merged.method ?? input.method;
    if (!merged.headers) {
      merged.headers = input.headers;
    }
    if (merged.body == null && !input.bodyUsed) {
      merged.body = input.body;
    }
  }
  return merged;
}

function wantsStreaming(_init) {
  return true;
}

function cleanupPending(id, state) {
  pending.delete(id);
  if (state?.signal && state.abortHandler) {
    state.signal.removeEventListener("abort", state.abortHandler);
  }
}

function rejectPending(id, error) {
  const state = pending.get(id);
  if (!state) {
    return;
  }
  cleanupPending(id, state);
  state.reject(error);
}

function resolveStreamingResponse(id, statusCode, headersRecord) {
  const state = pending.get(id);
  if (!state) {
    return;
  }
  const stream = new ReadableStream({
    start(controller) {
      state.controller = controller;
    },
  });
  state.responseStarted = true;
  state.resolve(
    new Response(stream, {
      status: statusCode,
      headers: recordToHeaders(headersRecord),
    }),
  );
}

function resolveBufferedResponse(id, statusCode, headersRecord, bodyBase64) {
  const state = pending.get(id);
  if (!state) {
    return;
  }
  cleanupPending(id, state);
  const body = bodyBase64 ? Buffer.from(bodyBase64, "base64") : undefined;
  state.resolve(
    new Response(body, {
      status: statusCode,
      headers: recordToHeaders(headersRecord),
    }),
  );
}

export function dispatchHostHttpReply(line) {
  const trimmed = line.trim();
  if (!trimmed) {
    return false;
  }
  let message;
  try {
    message = JSON.parse(trimmed);
  } catch {
    return false;
  }
  if (!message?.op || !String(message.op).startsWith("http_")) {
    return false;
  }

  const state = pending.get(message.id);
  if (!state) {
    return true;
  }

  switch (message.op) {
    case "http_response":
      resolveBufferedResponse(message.id, message.status_code ?? 0, message.headers, message.body);
      return true;
    case "http_headers":
      resolveStreamingResponse(message.id, message.status_code ?? 0, message.headers);
      return true;
    case "http_chunk": {
      if (!state.responseStarted || !state.controller) {
        rejectPending(message.id, new Error("cursor sidecar http stream started without headers"));
        return true;
      }
      if (message.payload) {
        state.controller.enqueue(Buffer.from(message.payload, "base64"));
      }
      return true;
    }
    case "http_end":
      if (state.controller) {
        state.controller.close();
      }
      cleanupPending(message.id, state);
      return true;
    case "http_error":
      rejectPending(message.id, new Error(message.message || "cursor sidecar http bridge failed"));
      return true;
    default:
      return false;
  }
}

export function installHostFetchBridge() {
  const originalFetch = globalThis.fetch?.bind(globalThis);
  if (typeof originalFetch !== "function") {
    throw new Error("global fetch is required for the cursor sidecar");
  }

  globalThis.fetch = async (input, init = {}) => {
    const url = resolveRequestURL(input);
    if (!url || (url.protocol !== "http:" && url.protocol !== "https:")) {
      return originalFetch(input, init);
    }

    const merged = mergeInit(input, init);
    const method = (merged.method ?? "GET").toUpperCase();
    const body = await readBody(input, merged);
    const stream = wantsStreaming(merged);
    const id = String(++seq);

    return new Promise((resolve, reject) => {
      const state = {
        resolve,
        reject,
        responseStarted: false,
        controller: null,
        signal: merged.signal,
        abortHandler: null,
      };
      pending.set(id, state);

      if (merged.signal) {
        if (merged.signal.aborted) {
          cleanupPending(id, state);
          reject(new DOMException("The operation was aborted.", "AbortError"));
          return;
        }
        state.abortHandler = () => {
          rejectPending(id, new DOMException("The operation was aborted.", "AbortError"));
        };
        merged.signal.addEventListener("abort", state.abortHandler, { once: true });
      }

      emit({
        id,
        event: "http",
        method,
        url: url.href,
        headers: headersToRecord(merged.headers),
        body: body ? Buffer.from(body).toString("base64") : "",
        stream,
      });
    });
  };
}
