import { afterEach, describe, expect, mock, test } from "bun:test"
import { Buffer } from "node:buffer"

mock.module("cloudflare:workers", () => ({ DurableObject: class {} }))
mock.module("@subrouter/core", () => ({
  accountHasQuotaForModel: () => true,
  quotaKeyForModel: (model: string | undefined) => model ?? "default",
}))

const { __test } = await import("../src/index.ts")

const proxyUpstream = __test.proxyUpstream as unknown as (
  request: Request,
  env: Record<string, unknown>,
  actor: Record<string, unknown>,
  routeInput: Record<string, unknown>,
  telemetry: undefined,
  waitUntil: (promise: Promise<unknown>) => void
) => Promise<Response>
const originalFetch = globalThis.fetch
const encoder = new TextEncoder()
const decoder = new TextDecoder()

afterEach(() => {
  globalThis.fetch = originalFetch
})

describe("HTTP proxy streaming transcripts", () => {
  test("passes through the first upstream chunk while transcript body RPC is pending", async () => {
    let upstreamController: ReadableStreamDefaultController<Uint8Array> | undefined
    const waitUntilPromises: Promise<unknown>[] = []
    const bodyRecordStarted = deferred<void>()
    const bodyRecordBlocked = deferred<void>()
    const actor = fakeActor({
      async recordTranscriptBody(input) {
        if (input.direction === "client_to_upstream") {
          bodyRecordStarted.resolve()
          await bodyRecordBlocked.promise
        }
        return { ok: true }
      },
    })

    setFetch(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              upstreamController = controller
              controller.enqueue(encoder.encode("first\n"))
            },
          }),
          { status: 200 }
        )
    )

    const response = await proxyUpstream(
      proxyRequest({ body: "client-body", sessionId: "stream-session" }),
      fakeEnv(),
      actor,
      routeInput("stream-session"),
      undefined,
      (promise: Promise<unknown>) => waitUntilPromises.push(promise)
    )

    expect(response.status).toBe(200)
    await withTimeout(bodyRecordStarted.promise, "body recording did not start")
    const reader = response.body?.getReader()
    expect(reader).toBeDefined()
    const first = await withTimeout(reader!.read(), "first response chunk")
    expect(decoder.decode(first.value)).toBe("first\n")
    expect(first.done).toBe(false)

    bodyRecordBlocked.resolve()
    upstreamController?.enqueue(encoder.encode("last\n"))
    upstreamController?.close()
    const second = await withTimeout(reader!.read(), "second response chunk")
    const done = await withTimeout(reader!.read(), "response stream close")
    expect(decoder.decode(second.value)).toBe("last\n")
    expect(done.done).toBe(true)
    await Promise.all(waitUntilPromises)
  })

  test("records streamed HTTP transcript events in order with body hashes", async () => {
    const events: TranscriptEvent[] = []
    const waitUntilPromises: Promise<unknown>[] = []
    const actor = fakeActor({
      async recordTranscriptMeta(input) {
        events.push({ kind: "meta", input })
        return { ok: true }
      },
      async recordTranscriptBody(input) {
        events.push({ kind: "body", input })
        return { ok: true }
      },
    })

    setFetch(async () => new Response("upstream-body", { status: 201 }))
    const response = await proxyUpstream(
      proxyRequest({ body: "client-body", sessionId: "integrity-session" }),
      fakeEnv(),
      actor,
      routeInput("integrity-session"),
      undefined,
      (promise: Promise<unknown>) => waitUntilPromises.push(promise)
    )

    expect(response.status).toBe(201)
    expect(await response.text()).toBe("upstream-body")
    await Promise.all(waitUntilPromises)

    expect(events.map((event) => event.kind)).toEqual(["meta", "body", "body"])
    expect(events[0]?.input.payload).toMatchObject({
      transport: "http",
      account: "acct-test",
      method: "POST",
      path: "/v1/responses",
    })
    expect(events[1]?.input).toMatchObject({
      direction: "client_to_upstream",
      bytes: byteLength("client-body"),
      bodyBase64: base64("client-body"),
      sha256: await sha256("client-body"),
    })
    expect(events[2]?.input).toMatchObject({
      direction: "upstream_to_client",
      bytes: byteLength("upstream-body"),
      bodyBase64: base64("upstream-body"),
      sha256: await sha256("upstream-body"),
      payload: { status: 201 },
    })
  })

  test("redacts upstream query values in transcript meta without changing the upstream request", async () => {
    const events: TranscriptEvent[] = []
    const waitUntilPromises: Promise<unknown>[] = []
    let fetchedURL = ""
    const actor = fakeActor({
      async recordTranscriptMeta(input) {
        events.push({ kind: "meta", input })
        return { ok: true }
      },
      async recordTranscriptBody(input) {
        events.push({ kind: "body", input })
        return { ok: true }
      },
    })

    setFetch(async (input) => {
      fetchedURL = new Request(input).url
      return new Response("ok", { status: 200 })
    })
    const response = await proxyUpstream(
      proxyRequest({
        body: "client-body",
        sessionId: "redacted-query-session",
        query: "?api_key=sk-secret123&debug=true",
      }),
      fakeEnv(),
      actor,
      routeInput("redacted-query-session"),
      undefined,
      (promise: Promise<unknown>) => waitUntilPromises.push(promise)
    )

    expect(response.status).toBe(200)
    await response.text()
    await Promise.all(waitUntilPromises)

    expect(fetchedURL).toBe(
      "https://upstream.example/backend-api/codex/responses?api_key=sk-secret123&debug=true"
    )
    expect(events[0]?.input.payload.upstream).toBe(
      "https://upstream.example/backend-api/codex/responses?api_key=[redacted]&debug=[redacted]"
    )
    expect(JSON.stringify(events[0]?.input.payload)).toContain("api_key")
    expect(JSON.stringify(events[0]?.input.payload)).not.toContain("sk-secret123")
  })

  test("isolates deferred transcript RPC failures from the client response", async () => {
    const waitUntilPromises: Promise<unknown>[] = []
    const actor = fakeActor({
      async recordTranscriptBody() {
        throw new Error("transcript write failed")
      },
    })

    setFetch(async () => new Response("still returned", { status: 202 }))
    const response = await proxyUpstream(
      proxyRequest({ body: "client-body", sessionId: "failure-session" }),
      fakeEnv(),
      actor,
      routeInput("failure-session"),
      undefined,
      (promise: Promise<unknown>) => waitUntilPromises.push(promise)
    )

    expect(response.status).toBe(202)
    expect(await response.text()).toBe("still returned")
    await expect(Promise.all(waitUntilPromises)).resolves.toEqual([undefined])
  })

  test("drains the response tee when transcript meta recording rejects", async () => {
    const waitUntilPromises: Promise<unknown>[] = []
    const upstreamDrained = deferred<void>()
    let chunk = 0
    const actor = fakeActor({
      async recordTranscriptMeta() {
        throw new Error("meta write failed")
      },
    })

    setFetch(
      async () =>
        new Response(
          new ReadableStream<Uint8Array>({
            pull(controller) {
              chunk += 1
              if (chunk <= 3) {
                controller.enqueue(encoder.encode(`chunk-${chunk}\n`))
                return
              }
              controller.close()
              upstreamDrained.resolve()
            },
          }),
          { status: 200 }
        )
    )

    const response = await proxyUpstream(
      proxyRequest({ body: "client-body", sessionId: "meta-failure-session" }),
      fakeEnv(),
      actor,
      routeInput("meta-failure-session"),
      undefined,
      (promise: Promise<unknown>) => waitUntilPromises.push(promise)
    )

    expect(response.status).toBe(200)
    await expect(Promise.all(waitUntilPromises)).resolves.toEqual([undefined])
    await withTimeout(upstreamDrained.promise, "response tee drain")
    await response.body?.cancel()
  })

  test("handles rejected request body recording promises before fetch fails", async () => {
    const unhandled: unknown[] = []
    const onUnhandled = (reason: unknown): void => {
      unhandled.push(reason)
    }
    process.on("unhandledRejection", onUnhandled)
    const actor = fakeActor({})
    const request = new Request("https://subrouter.example/v1/responses", {
      method: "POST",
      headers: {
        Authorization: "Bearer tenant-key",
        "Content-Type": "application/octet-stream",
        "X-Subrouter-Session": "request-abort-session",
      },
      body: new ReadableStream<Uint8Array>({
        start(controller) {
          queueMicrotask(() => controller.error(new Error("client aborted upload")))
        },
      }),
      duplex: "half",
    } as RequestInit & { duplex: "half" })

    setFetch(async () => {
      throw new Error("upstream fetch failed")
    })
    try {
      await expect(
        proxyUpstream(
          request,
          fakeEnv(),
          actor,
          routeInput("request-abort-session"),
          undefined,
          () => {}
        )
      ).rejects.toThrow("upstream fetch failed")
      await Bun.sleep(20)
      expect(unhandled).toEqual([])
    } finally {
      process.off("unhandledRejection", onUnhandled)
    }
  })

  test("records an empty upstream body for null-body 204 responses", async () => {
    const events: TranscriptEvent[] = []
    const waitUntilPromises: Promise<unknown>[] = []
    const actor = fakeActor({
      async recordTranscriptMeta(input) {
        events.push({ kind: "meta", input })
        return { ok: true }
      },
      async recordTranscriptBody(input) {
        events.push({ kind: "body", input })
        return { ok: true }
      },
    })

    setFetch(async () => new Response(null, { status: 204 }))
    const response = await proxyUpstream(
      proxyRequest({ method: "GET", sessionId: "null-body-session" }),
      fakeEnv(),
      actor,
      routeInput("null-body-session"),
      undefined,
      (promise: Promise<unknown>) => waitUntilPromises.push(promise)
    )

    expect(response.status).toBe(204)
    expect(await response.text()).toBe("")
    await Promise.all(waitUntilPromises)

    expect(events.map((event) => event.kind)).toEqual(["meta", "body"])
    expect(events[1]?.input).toMatchObject({
      direction: "upstream_to_client",
      bytes: 0,
      bodyBase64: "",
      sha256: await sha256(""),
      payload: { status: 204 },
    })
  })
})

interface TranscriptEvent {
  readonly kind: "meta" | "body"
  readonly input: Record<string, any>
}

const fakeEnv = (): Record<string, unknown> => ({
  CODEX_UPSTREAM: "https://upstream.example/backend-api/codex",
})

const routeInput = (sessionId: string): Record<string, unknown> => ({
  orgId: "org-test",
  agentType: "codex",
  sessionId,
})

const proxyRequest = (input: {
  readonly method?: string
  readonly body?: string
  readonly sessionId: string
  readonly query?: string
}): Request =>
  new Request(`https://subrouter.example/v1/responses${input.query ?? ""}`, {
    method: input.method ?? "POST",
    headers: {
      Authorization: "Bearer tenant-key",
      "Content-Type": "application/json",
      "X-Subrouter-Session": input.sessionId,
    },
    ...(input.body === undefined ? {} : { body: input.body }),
  })

const fakeActor = (overrides: {
  readonly recordTranscriptMeta?: (input: Record<string, any>) => Promise<{ ok: true }>
  readonly recordTranscriptBody?: (input: Record<string, any>) => Promise<{ ok: true }>
}): Record<string, unknown> => ({
  async routeForProxy() {
    return {
      account: {
        id: "acct-test",
        kind: "codex_oauth",
        label: "test@example.com",
        hasCredentials: true,
        hasTotp: false,
        credentials: { accessToken: "upstream-token" },
      },
    }
  },
  recordTranscriptMeta:
    overrides.recordTranscriptMeta ?? (async () => ({ ok: true })),
  recordTranscriptBody:
    overrides.recordTranscriptBody ?? (async () => ({ ok: true })),
})

type TestFetch = (
  input: Parameters<typeof fetch>[0],
  init?: Parameters<typeof fetch>[1]
) => Response | Promise<Response>

const setFetch = (fetcher: TestFetch): void => {
  globalThis.fetch = Object.assign(fetcher, { preconnect: () => {} }) as typeof fetch
}

const deferred = <T>(): {
  readonly promise: Promise<T>
  readonly resolve: (value: T | PromiseLike<T>) => void
  readonly reject: (reason?: unknown) => void
} => {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((innerResolve, innerReject) => {
    resolve = innerResolve
    reject = innerReject
  })
  return { promise, resolve, reject }
}

const withTimeout = async <T>(promise: Promise<T>, label: string): Promise<T> => {
  let timeout: Timer | undefined
  try {
    return await Promise.race([
      promise,
      new Promise<T>((_, reject) => {
        timeout = setTimeout(() => reject(new Error(`${label} timed out`)), 1_000)
      }),
    ])
  } finally {
    if (timeout) clearTimeout(timeout)
  }
}

const byteLength = (value: string): number => encoder.encode(value).byteLength

const base64 = (value: string): string => Buffer.from(value).toString("base64")

const sha256 = async (value: string): Promise<string> => {
  const digest = await crypto.subtle.digest("SHA-256", encoder.encode(value))
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("")
}
