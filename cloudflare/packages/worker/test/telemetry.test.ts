import { describe, expect, test } from "bun:test"
import {
  buildOtlpTraceBody,
  buildSpanAttributes,
  createRequestTelemetryContext,
  exportAxiomSpan,
  routePatternForRequest,
  scheduleAxiomSpanExport,
  type OtlpAttribute,
} from "../src/telemetry.ts"

const attrValue = (
  attributes: ReadonlyArray<OtlpAttribute>,
  key: string
): string | number | boolean | undefined => {
  const value = attributes.find((attribute) => attribute.key === key)?.value
  if (!value) return undefined
  if ("stringValue" in value) return value.stringValue
  if ("intValue" in value) return value.intValue
  if ("doubleValue" in value) return value.doubleValue
  return value.boolValue
}

describe("Axiom telemetry", () => {
  test("classifies route patterns without raw account or tenant ids", () => {
    expect(routePatternForRequest(new Request("https://subrouter.test/v1/responses"))).toBe("/proxy/codex")
    expect(routePatternForRequest(new Request("https://subrouter.test/v1/messages"))).toBe("/proxy/claude")
    expect(
      routePatternForRequest(new Request("https://subrouter.test/v1/responses"), {
        websocket: true,
      })
    ).toBe("/proxy/websocket")
    expect(routePatternForRequest(new Request("https://subrouter.test/route"))).toBe("/route")
    expect(routePatternForRequest(new Request("https://subrouter.test/admin/tenants/acme/rotate"))).toBe(
      "/admin/tenants/:id/rotate"
    )
    expect(routePatternForRequest(new Request("https://subrouter.test/tenant/accounts/claude-123"))).toBe(
      "/tenant/accounts/:id"
    )
  })

  test("builds auth and routed-account span attributes", () => {
    const attributes = buildSpanAttributes({
      method: "POST",
      routePattern: "/proxy/claude",
      statusCode: 200,
      durationMs: 12.5,
      environment: "staging",
      tenantId: "tenant-a",
      auth: "tenant",
      accountId: "claude-1",
      accountKind: "anthropic_oauth",
      upstreamProvider: "claude",
      upstreamStatus: 200,
    })

    expect(attrValue(attributes, "service.name")).toBe("subrouter-do")
    expect(attrValue(attributes, "deployment.environment")).toBe("staging")
    expect(attrValue(attributes, "http.request.method")).toBe("POST")
    expect(attrValue(attributes, "url.path")).toBe("/proxy/claude")
    expect(attrValue(attributes, "http.response.status_code")).toBe("200")
    expect(attrValue(attributes, "duration_ms")).toBe(12.5)
    expect(attrValue(attributes, "subrouter.tenant_id")).toBe("tenant-a")
    expect(attrValue(attributes, "subrouter.auth")).toBe("tenant")
    expect(attrValue(attributes, "subrouter.account_id")).toBe("claude-1")
    expect(attrValue(attributes, "subrouter.account_kind")).toBe("anthropic_oauth")
    expect(attrValue(attributes, "subrouter.upstream_provider")).toBe("claude")
    expect(attrValue(attributes, "subrouter.upstream_status_code")).toBe("200")
  })

  test("adds error.type for explicit errors and 5xx responses", () => {
    expect(
      attrValue(
        buildSpanAttributes({
          method: "GET",
          routePattern: "/status",
          statusCode: 400,
          durationMs: 1,
          auth: "legacy",
          errorType: "TypeError",
        }),
        "error.type"
      )
    ).toBe("TypeError")
    expect(
      attrValue(
        buildSpanAttributes({
          method: "GET",
          routePattern: "/status",
          statusCode: 503,
          durationMs: 1,
          auth: "none",
        }),
        "error.type"
      )
    ).toBe("HttpError")
  })

  test("does not schedule or fetch when Axiom env is incomplete", async () => {
    let fetchCount = 0
    let waitUntilCount = 0
    const request = new Request("https://subrouter.test/healthz")
    const telemetry = createRequestTelemetryContext(request)

    for (const env of [{ AXIOM_TOKEN: "token" }, { AXIOM_DATASET: "dataset" }, {}]) {
      scheduleAxiomSpanExport(
        { waitUntil: () => { waitUntilCount += 1 } },
        {
          env,
          request,
          telemetry,
          response: new Response("ok"),
          startTimeMs: 1,
          endTimeMs: 2,
          fetcher: (async () => {
            fetchCount += 1
            return new Response("unexpected")
          }) as unknown as typeof fetch,
        }
      )
    }

    await Promise.resolve()
    expect(waitUntilCount).toBe(0)
    expect(fetchCount).toBe(0)
  })

  test("exports one OTLP HTTP JSON POST with Axiom headers", async () => {
    const calls: Array<{ readonly input: RequestInfo | URL; readonly init?: RequestInit }> = []
    const request = new Request("https://subrouter.test/healthz")
    const telemetry = createRequestTelemetryContext(request)
    const waitUntilTasks: Promise<unknown>[] = []

    scheduleAxiomSpanExport(
      {
        waitUntil: (task) => {
          waitUntilTasks.push(task)
        },
      },
      {
        env: {
          AXIOM_TOKEN: "axiom-token",
          AXIOM_DATASET: "otel-traces",
          AXIOM_DOMAIN: "axiom.example",
          SUBROUTER_ENV: "production",
        },
        request,
        telemetry,
        response: new Response("ok", { status: 204 }),
        startTimeMs: 1_000,
        endTimeMs: 1_025,
        fetcher: (async (input, init) => {
          calls.push({ input, init })
          return new Response("ok", { status: 200 })
        }) as typeof fetch,
      }
    )

    expect(waitUntilTasks).toHaveLength(1)
    await waitUntilTasks[0]
    expect(calls).toHaveLength(1)
    expect(calls[0]?.input).toBe("https://axiom.example/v1/traces")
    expect(calls[0]?.init?.method).toBe("POST")
    const headers = new Headers(calls[0]?.init?.headers)
    expect(headers.get("Authorization")).toBe("Bearer axiom-token")
    expect(headers.get("X-Axiom-Dataset")).toBe("otel-traces")
    expect(headers.get("Content-Type")).toBe("application/json")
    const body = JSON.parse(String(calls[0]?.init?.body))
    expect(body.resourceSpans[0].scopeSpans[0].spans).toHaveLength(1)
    expect(
      body.resourceSpans[0].scopeSpans[0].spans[0].attributes.some(
        (attribute: OtlpAttribute) => attribute.key === "duration_ms"
      )
    ).toBe(true)
  })

  test("export failures are swallowed by waitUntil task", async () => {
    const request = new Request("https://subrouter.test/healthz")
    const waitUntilTasks: Promise<unknown>[] = []
    scheduleAxiomSpanExport(
      {
        waitUntil: (task) => {
          waitUntilTasks.push(task)
        },
      },
      {
        env: { AXIOM_TOKEN: "token", AXIOM_DATASET: "dataset" },
        request,
        telemetry: createRequestTelemetryContext(request),
        response: new Response("still ok", { status: 200 }),
        startTimeMs: 1,
        endTimeMs: 2,
        fetcher: (async () => {
          throw new TypeError("network down")
        }) as unknown as typeof fetch,
      }
    )

    await expect(waitUntilTasks[0]).resolves.toBeUndefined()
  })

  test("serialized span JSON does not contain tenant keys, Anthropic keys, or Bearer material", async () => {
    const request = new Request("https://subrouter.test/tenant/accounts/sk-ant-secret?key=srt_secret", {
      method: "DELETE",
      headers: {
        Authorization: "Bearer srt_0123456789abcdef0123456789abcdef",
        "X-Subrouter-Tenant-Key": "srt_0123456789abcdef0123456789abcdef",
        "X-Test-Api-Key": "sk-ant-test",
      },
      body: "Bearer srt_body sk-ant-body",
    })
    const telemetry = createRequestTelemetryContext(request)
    telemetry.auth = "tenant"
    telemetry.tenantId = "srt_tenant"
    telemetry.accountId = "sk-ant-account"
    telemetry.accountKind = "Bearer kind"
    telemetry.upstreamProvider = "claude"

    const bodies: string[] = []
    await exportAxiomSpan({
      env: { AXIOM_TOKEN: "token", AXIOM_DATASET: "dataset" },
      request,
      telemetry,
      response: new Response("ok", { status: 200 }),
      startTimeMs: 1,
      endTimeMs: 2,
      fetcher: (async (_input, init) => {
        bodies.push(String(init?.body))
        return new Response("ok", { status: 200 })
      }) as typeof fetch,
    })

    expect(bodies).toHaveLength(1)
    expect(bodies[0]).not.toContain("srt_")
    expect(bodies[0]).not.toContain("sk-ant-")
    expect(bodies[0]).not.toContain("Bearer ")
  })

  test("OTLP body uses hex ids and nano timestamps", () => {
    const body = buildOtlpTraceBody({
      traceId: "0".repeat(32),
      spanId: "1".repeat(16),
      startTimeMs: 1_000,
      endTimeMs: 1_001,
      span: {
        method: "GET",
        routePattern: "/healthz",
        statusCode: 200,
        durationMs: 1,
        environment: "staging",
        auth: "none",
      },
    })
    const span = body.resourceSpans[0]!.scopeSpans[0]!.spans[0]!
    expect(span.traceId).toBe("0".repeat(32))
    expect(span.spanId).toBe("1".repeat(16))
    expect(span.startTimeUnixNano).toBe("1000000000")
    expect(span.endTimeUnixNano).toBe("1001000000")
  })
})
