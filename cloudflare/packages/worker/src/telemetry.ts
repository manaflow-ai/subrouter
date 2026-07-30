export type SubrouterAuthClassification = "tenant" | "admin" | "legacy" | "none"

export interface AxiomTelemetryEnv {
  readonly AXIOM_TOKEN?: string
  readonly AXIOM_DATASET?: string
  readonly AXIOM_DOMAIN?: string
  readonly SUBROUTER_ENV?: string
}

export interface MutableRequestTelemetryContext {
  routePattern: string
  tenantId?: string
  auth: SubrouterAuthClassification
  accountId?: string
  accountKind?: string
  upstreamProvider?: string
  upstreamStatus?: number
  errorType?: string
}

export interface RequestSpanInput {
  readonly method: string
  readonly routePattern: string
  readonly statusCode: number
  readonly durationMs: number
  readonly environment?: string
  readonly tenantId?: string
  readonly auth: SubrouterAuthClassification
  readonly accountId?: string
  readonly accountKind?: string
  readonly upstreamProvider?: string
  readonly upstreamStatus?: number
  readonly errorType?: string
}

export interface TraceExportInput {
  readonly env: AxiomTelemetryEnv
  readonly request: Request
  readonly telemetry: MutableRequestTelemetryContext
  readonly response: Response
  readonly startTimeMs: number
  readonly endTimeMs?: number
  readonly fetcher?: typeof fetch
}

type OtlpAttributeValue =
  | { readonly stringValue: string }
  | { readonly intValue: string }
  | { readonly doubleValue: number }
  | { readonly boolValue: boolean }

export interface OtlpAttribute {
  readonly key: string
  readonly value: OtlpAttributeValue
}

interface OtlpSpan {
  readonly traceId: string
  readonly spanId: string
  readonly name: string
  readonly kind: number
  readonly startTimeUnixNano: string
  readonly endTimeUnixNano: string
  readonly attributes: OtlpAttribute[]
  readonly status?: { readonly code: number }
}

interface OtlpBody {
  readonly resourceSpans: ReadonlyArray<{
    readonly resource: { readonly attributes: OtlpAttribute[] }
    readonly scopeSpans: ReadonlyArray<{
      readonly scope: { readonly name: string }
      readonly spans: ReadonlyArray<OtlpSpan>
    }>
  }>
}

const defaultAxiomDomain = "api.axiom.co"
const serviceName = "subrouter-do"

export const isAxiomTelemetryConfigured = (env: AxiomTelemetryEnv): boolean =>
  nonEmpty(env.AXIOM_TOKEN) !== null && nonEmpty(env.AXIOM_DATASET) !== null

export const createRequestTelemetryContext = (
  request: Request
): MutableRequestTelemetryContext => ({
  routePattern: routePatternForRequest(request),
  auth: "none",
})

export const routePatternForRequest = (
  request: Request,
  options: { readonly upstreamProvider?: string; readonly websocket?: boolean } = {}
): string => {
  const url = new URL(request.url)
  const path = url.pathname
  if (options.websocket && path !== "/ws") return "/proxy/websocket"
  if (options.upstreamProvider === "claude") return "/proxy/claude"
  if (options.upstreamProvider === "codex") return "/proxy/codex"

  if (path === "/healthz") return "/healthz"
  if (path === "/_subrouter/health") return "/_subrouter/health"
  if (path === "/_subrouter/ready") return "/_subrouter/ready"
  if (path === "/ws") return "/ws"
  if (path === "/websocket-status") return "/websocket-status"
  if (path === "/route") return "/route"
  if (path === "/usage") return "/usage"
  if (path === "/status") return "/status"
  if (path === "/tenant/accounts") return "/tenant/accounts"
  if (/^\/tenant\/accounts\/[^/]+\/repair$/.test(path)) {
    return "/tenant/accounts/:id/repair"
  }
  if (/^\/tenant\/accounts\/[^/]+$/.test(path)) return "/tenant/accounts/:id"
  if (path === "/tenant/leases") return "/tenant/leases"
  if (/^\/tenant\/leases\/[^/]+\/events$/.test(path)) {
    return "/tenant/leases/:id/events"
  }
  if (path === "/admin/tenants") return "/admin/tenants"
  if (/^\/admin\/tenants\/[^/]+\/revoke$/.test(path)) return "/admin/tenants/:id/revoke"
  if (/^\/admin\/tenants\/[^/]+\/rotate$/.test(path)) return "/admin/tenants/:id/rotate"
  if (path === "/admin/accounts") return "/admin/accounts"
  if (/^\/admin\/accounts\/[^/]+\/totp$/.test(path)) return "/admin/accounts/:id/totp"
  if (path === "/admin/model-probe") return "/admin/model-probe"
  if (path === "/_subrouter/drain") return "/_subrouter/drain"
  if (path === "/_subrouter/drain-status") return "/_subrouter/drain-status"
  if (path === "/_subrouter/sessions") return "/_subrouter/sessions"
  if (path === "/_subrouter/accounts") return "/_subrouter/accounts"
  if (path === "/_subrouter/account-status") return "/_subrouter/account-status"
  if (path === "/_subrouter/usage-status") return "/_subrouter/usage-status"
  if (path === "/_subrouter/reload-accounts") return "/_subrouter/reload-accounts"
  if (path === "/_subrouter/dashboard") return "/_subrouter/dashboard"
  if (path === "/_subrouter/transcripts") return "/_subrouter/transcripts"
  if (/^\/_subrouter\/transcripts\/[^/]+\/[^/]+\/raw$/.test(path)) {
    return "/_subrouter/transcripts/:agent/:session/raw"
  }
  if (/^\/_subrouter\/transcripts\/[^/]+\/[^/]+$/.test(path)) {
    return "/_subrouter/transcripts/:agent/:session"
  }
  if (path.startsWith("/admin/")) return "/admin/*"
  if (path.startsWith("/tenant/")) return "/tenant/*"
  if (path.startsWith("/_subrouter/")) return "/_subrouter/*"
  if (path === "/v1/messages" || path.startsWith("/v1/messages/")) {
    return "/proxy/claude"
  }
  return "/proxy/codex"
}

export const errorTypeFor = (error: unknown): string => {
  if (error && typeof error === "object") {
    const name = (error as { readonly name?: unknown }).name
    if (typeof name === "string" && name.trim()) return name
    const constructorName = (error as { readonly constructor?: { readonly name?: string } }).constructor?.name
    if (constructorName) return constructorName
  }
  return "Error"
}

export const buildSpanAttributes = (input: RequestSpanInput): OtlpAttribute[] => {
  const errorType =
    input.errorType ?? (input.statusCode >= 500 ? "HttpError" : undefined)
  return [
    stringAttribute("service.name", serviceName),
    stringAttribute("deployment.environment", input.environment ?? "unknown"),
    stringAttribute("http.request.method", input.method),
    stringAttribute("url.path", input.routePattern),
    intAttribute("http.response.status_code", input.statusCode),
    doubleAttribute("duration", input.durationMs),
    doubleAttribute("duration_ms", input.durationMs),
    stringAttribute("subrouter.auth", input.auth),
    ...optionalStringAttribute("subrouter.tenant_id", input.tenantId),
    ...optionalStringAttribute("subrouter.account_id", input.accountId),
    ...optionalStringAttribute("subrouter.account_kind", input.accountKind),
    ...optionalStringAttribute("subrouter.upstream_provider", input.upstreamProvider),
    ...optionalIntAttribute("subrouter.upstream_status_code", input.upstreamStatus),
    ...optionalStringAttribute("error.type", errorType),
  ]
}

export const buildOtlpTraceBody = (input: {
  readonly span: RequestSpanInput
  readonly startTimeMs: number
  readonly endTimeMs: number
  readonly traceId?: string
  readonly spanId?: string
}): OtlpBody => {
  const attributes = buildSpanAttributes(input.span)
  const resourceAttributes = attributes.filter(
    (attribute) =>
      attribute.key === "service.name" ||
      attribute.key === "deployment.environment"
  )
  const span: OtlpSpan = {
    traceId: input.traceId ?? randomHex(16),
    spanId: input.spanId ?? randomHex(8),
    name: `${input.span.method} ${input.span.routePattern}`,
    kind: 2,
    startTimeUnixNano: msToUnixNano(input.startTimeMs),
    endTimeUnixNano: msToUnixNano(input.endTimeMs),
    attributes,
    ...(input.span.statusCode >= 500 || input.span.errorType
      ? { status: { code: 2 } }
      : {}),
  }
  return {
    resourceSpans: [
      {
        resource: { attributes: resourceAttributes },
        scopeSpans: [
          {
            scope: { name: "subrouter.worker" },
            spans: [span],
          },
        ],
      },
    ],
  }
}

export const exportAxiomSpan = async (input: TraceExportInput): Promise<void> => {
  const token = nonEmpty(input.env.AXIOM_TOKEN)
  const dataset = nonEmpty(input.env.AXIOM_DATASET)
  if (!token || !dataset) return

  const endTimeMs = input.endTimeMs ?? Date.now()
  const durationMs = Math.max(0, endTimeMs - input.startTimeMs)
  const body = buildOtlpTraceBody({
    span: {
      method: input.request.method,
      routePattern: input.telemetry.routePattern,
      statusCode: input.response.status,
      durationMs,
      environment: input.env.SUBROUTER_ENV,
      tenantId: input.telemetry.tenantId,
      auth: input.telemetry.auth,
      accountId: input.telemetry.accountId,
      accountKind: input.telemetry.accountKind,
      upstreamProvider: input.telemetry.upstreamProvider,
      upstreamStatus: input.telemetry.upstreamStatus,
      errorType: input.telemetry.errorType,
    },
    startTimeMs: input.startTimeMs,
    endTimeMs,
  })
  const domain = nonEmpty(input.env.AXIOM_DOMAIN) ?? defaultAxiomDomain
  const fetcher = input.fetcher ?? fetch
  const response = await fetcher(`https://${domain}/v1/traces`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
      "X-Axiom-Dataset": dataset,
    },
    body: JSON.stringify(body),
  })
  await response.arrayBuffer().catch(() => undefined)
}

export const scheduleAxiomSpanExport = (
  ctx: Pick<ExecutionContext, "waitUntil"> | undefined,
  input: TraceExportInput
): void => {
  if (!isAxiomTelemetryConfigured(input.env)) return
  const task = Promise.resolve()
    .then(() => exportAxiomSpan(input))
    .catch(() => undefined)
  if (ctx) {
    ctx.waitUntil(task)
    return
  }
  void task
}

const stringAttribute = (key: string, value: string): OtlpAttribute => ({
  key,
  value: { stringValue: sanitizeAttributeString(value) },
})

const intAttribute = (key: string, value: number): OtlpAttribute => ({
  key,
  value: { intValue: String(Math.trunc(value)) },
})

const doubleAttribute = (key: string, value: number): OtlpAttribute => ({
  key,
  value: { doubleValue: value },
})

const optionalStringAttribute = (
  key: string,
  value: string | undefined
): OtlpAttribute[] => {
  const parsed = nonEmpty(value)
  return parsed ? [stringAttribute(key, parsed)] : []
}

const optionalIntAttribute = (
  key: string,
  value: number | undefined
): OtlpAttribute[] =>
  typeof value === "number" && Number.isFinite(value)
    ? [intAttribute(key, value)]
    : []

const nonEmpty = (value: string | undefined): string | null =>
  typeof value === "string" && value.trim().length > 0 ? value : null

const sanitizeAttributeString = (value: string): string =>
  /srt_|sk-ant-|Bearer /.test(value) ? "[redacted]" : value

const msToUnixNano = (ms: number): string => {
  const wholeMs = Math.trunc(ms)
  return String(BigInt(wholeMs) * 1_000_000n)
}

const randomHex = (byteLength: number): string => {
  const bytes = new Uint8Array(byteLength)
  crypto.getRandomValues(bytes)
  return [...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")
}
