import { DurableObject } from "cloudflare:workers"
import {
  accountHasQuotaForModel,
  quotaKeyForModel,
  type Account,
  type AccountModelQuotas,
} from "@subrouter/core"
import {
  authModeForAccount,
  credentialNeedsRefresh,
  fetchProviderUsage,
  isOAuthKind,
  nextRefreshAt,
  normalizeAgentType,
  parseProxyRouteInput,
  providerForAccount,
  refreshFailureFromError,
  refreshOAuthCredentials,
  safeGoAccount,
  setUpstreamAuthHeaders,
  stripSubrouterHeaders,
  stickySessionId,
  terminalRefreshFailure,
  upstreamURLForRequest,
  usageStatus,
  type AccountCredentials,
  type AccountKind,
  type AgentType,
  type StoredAccountContract,
} from "./contract.ts"
import {
  isReservedTenantId,
  isTenantKey,
  tenantKeySha256Hex,
  TenantRegistryDurableObject,
  type TenantResolution,
} from "./tenants.ts"

export { TenantRegistryDurableObject }

type TotpAlgorithm = "SHA-1" | "SHA-256" | "SHA-512"

const accountKinds = new Set<AccountKind>([
  "codex_oauth",
  "anthropic_oauth",
  "openai_apikey",
  "anthropic_apikey",
])

interface RouteInput {
  readonly orgId?: string
  readonly agentType?: AgentType
  readonly upstreamProvider?: UpstreamProvider
  readonly sessionId: string
  readonly userEmail?: string
  readonly preferAccountId?: string
  readonly model?: string
  readonly quotaKey?: string
}

interface UsageInput {
  readonly orgId?: string
  readonly sessionId: string
  readonly accountId: string
}

interface TotpConfig {
  readonly seed: string
  readonly digits?: number
  readonly period?: number
  readonly algorithm?: TotpAlgorithm
}

interface UpsertAccountInput {
  readonly id: string
  readonly orgId: string
  readonly kind: AccountKind
  readonly label: string
  readonly enabled?: boolean
  readonly rateLimitRemaining?: number
  readonly credentials?: AccountCredentials
  readonly totp?: TotpConfig
  readonly modelQuotas?: AccountModelQuotas
}

interface TenantAccountUploadInput {
  readonly validate: boolean
  readonly account: UpsertAccountInput
}

interface TenantAccountOutput {
  readonly id: string
  readonly provider: "codex" | "claude"
  readonly label: string
  readonly auth_mode: "oauth" | "apikey"
  readonly created_at: string
  readonly email?: string
  readonly validation?: "ok" | "failed"
}

type UpstreamProvider = "claude" | "codex"

const anthropicBootstrapModelQuotas: AccountModelQuotas = {
  opus: { remainingPercent: 100 },
  sonnet: { remainingPercent: 100 },
}

const codexBootstrapModelQuotas: AccountModelQuotas = {
  spark: { remainingPercent: 100 },
}

interface ModelProbeInput {
  readonly orgId?: string
  readonly accountId: string
  readonly model: string
  readonly prompt?: string
}

interface StoredAccount extends Account {
  readonly hasCredentials: boolean
  readonly hasTotp: boolean
  readonly totpDigits: number | null
  readonly totpPeriod: number | null
  readonly totpAlgorithm: TotpAlgorithm | null
  readonly createdAt: number
  readonly updatedAt: number
  readonly source?: string
}

interface RouteOutput {
  readonly account: StoredAccount
  readonly routedAt: number
  readonly quotaKey: string
}

interface StatusOutput {
  readonly routeCount: number
  readonly usageCount: number
  readonly accountCount: number
  readonly sessionCount: number
  readonly lastRoutedAt: number | null
  readonly lastAccountId: string | null
  readonly lastSessionId: string | null
  readonly draining: boolean
}

interface StatusRow {
  readonly [key: string]: SqlStorageValue
  readonly route_count: number
  readonly usage_count: number
  readonly last_routed_at: number | null
  readonly last_account_id: string | null
  readonly last_session_id: string | null
  readonly cursor: number
}

interface UsageRow {
  readonly [key: string]: SqlStorageValue
  readonly last_used_at: number
}

interface AccountRow {
  readonly [key: string]: SqlStorageValue
  readonly id: string
  readonly org_id: string
  readonly kind: string
  readonly label: string
  readonly enabled: number
  readonly rate_limit_remaining: number | null
  readonly credentials_json: string | null
  readonly totp_seed_base32: string | null
  readonly totp_digits: number | null
  readonly totp_period: number | null
  readonly totp_algorithm: string | null
  readonly created_at: number
  readonly updated_at: number
}

interface OAuthRefreshStatus {
  readonly id: string
  readonly provider: "codex" | "claude"
  readonly auth_mode: "oauth" | "apikey"
  readonly email?: string
  readonly source: string
  readonly auth_checked: boolean
  readonly auth_valid: boolean
  readonly refreshed?: boolean
  readonly error?: string
}

interface SessionAssignmentRow {
  readonly [key: string]: SqlStorageValue
  readonly org_id: string
  readonly agent_type: string
  readonly session_id: string
  readonly quota_key: string
  readonly account_id: string
  readonly user_email: string | null
  readonly created_at: number
  readonly updated_at: number
}

interface CountRow {
  readonly [key: string]: SqlStorageValue
  readonly count: number
}

interface ModelQuotaRow {
  readonly [key: string]: SqlStorageValue
  readonly quota_key: string
  readonly remaining_percent: number
  readonly resets_at: number | null
  readonly protected_below_percent: number | null
}

interface TranscriptEventInput {
  readonly orgId?: string | undefined
  readonly agentType: string
  readonly sessionId: string
  readonly type: string
  readonly payload: Record<string, unknown>
}

interface TranscriptBodyInput {
  readonly orgId?: string | undefined
  readonly agentType: string
  readonly sessionId: string
  readonly eventType: "http_body" | "websocket_message"
  readonly direction: "client_to_upstream" | "upstream_to_client"
  readonly bodyBase64: string
  readonly bytes: number
  readonly sha256: string
  readonly payload?: Record<string, unknown>
}

interface TranscriptReadInput {
  readonly orgId?: string | undefined
  readonly agentType: string
  readonly sessionId: string
  readonly raw: boolean
}

interface TranscriptEventRow {
  readonly [key: string]: SqlStorageValue
  readonly event_id: number
  readonly org_id: string
  readonly agent_type: string
  readonly session_id: string
  readonly timestamp: string
  readonly type: string
  readonly payload_json: string
  readonly size_bytes: number
}

interface TranscriptDashboardData {
  readonly summaries: ReadonlyArray<Record<string, unknown>>
  readonly analytics: TranscriptAnalytics
}

interface WebSocketAttachment {
  readonly id: string
  readonly orgId: string
  readonly connectedAt: number
  readonly lastMessageAt: number | null
  readonly messageCount: number
}

interface WebSocketStatsOutput {
  readonly connectionCount: number
  readonly connections: ReadonlyArray<WebSocketAttachment>
}

interface WebSocketClientMessage {
  readonly type?: unknown
  readonly requestId?: unknown
  readonly orgId?: unknown
  readonly agentType?: unknown
  readonly sessionId?: unknown
  readonly preferAccountId?: unknown
  readonly model?: unknown
  readonly quotaKey?: unknown
  readonly accountId?: unknown
}

interface WebSocketServerMessage {
  readonly type: string
  readonly requestId?: string
  readonly ok?: boolean
  readonly error?: string
  readonly connectionId?: string
  readonly connectionCount?: number
  readonly message?: unknown
  readonly messages?: ReadonlyArray<WebSocketServerMessage>
}

export interface Env {
  readonly SUBROUTER_DO: DurableObjectNamespace<SubrouterDurableObject>
  readonly TENANT_REGISTRY_DO: DurableObjectNamespace<TenantRegistryDurableObject>
  readonly ADMIN_TOKEN?: string
  readonly PROXY_TOKEN?: string
  readonly ALLOW_LEGACY_PROXY_TOKEN?: string
  readonly CODEX_UPSTREAM?: string
  readonly API_UPSTREAM?: string
  readonly CLAUDE_UPSTREAM?: string
  readonly VALIDATION_TIMEOUT_MS?: string
}

const json = (body: unknown, init?: ResponseInit): Response =>
  new Response(JSON.stringify(body), {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...init?.headers,
    },
  })

const errorJson = (error: unknown, status: number): Response =>
  json({ error: String((error as Error).message ?? error) }, { status })

const nonEmptyString = (value: unknown): string | null =>
  typeof value === "string" && value.trim().length > 0 ? value : null

const parseJsonRecord = async (
  request: Request
): Promise<Record<string, unknown> | Response> => {
  const body = await request.json().catch(() => null)
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    return json({ error: "Missing JSON body" }, { status: 400 })
  }
  return body as Record<string, unknown>
}

const parseRouteInput = async (request: Request): Promise<RouteInput> => {
  const body = await request.json().catch(() => ({}))
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    return { orgId: "demo-org", sessionId: "default" }
  }
  const record = body as Record<string, unknown>
  const input = {
    orgId: nonEmptyString(record["orgId"]) ?? "demo-org",
    agentType: nonEmptyString(record["agentType"]) ?? "codex",
    sessionId: nonEmptyString(record["sessionId"]) ?? "default",
  }
  const userEmail = nonEmptyString(record["userEmail"])
  const preferAccountId = nonEmptyString(record["preferAccountId"])
  const model = nonEmptyString(record["model"])
  const quotaKey = nonEmptyString(record["quotaKey"])
  const modelInput = {
    ...input,
    ...(userEmail ? { userEmail } : {}),
    ...(model ? { model } : {}),
    ...(quotaKey ? { quotaKey } : {}),
  }
  if (preferAccountId) {
    return { ...modelInput, preferAccountId }
  }
  return modelInput
}

const parseUsageInput = async (request: Request): Promise<Response | UsageInput> => {
  const record = await parseJsonRecord(request)
  if (record instanceof Response) return record

  const sessionId = nonEmptyString(record["sessionId"])
  if (!sessionId) {
    return json({ error: "Missing sessionId" }, { status: 400 })
  }
  const accountId = nonEmptyString(record["accountId"])
  if (!accountId) {
    return json({ error: "Missing accountId" }, { status: 400 })
  }

  const input = { sessionId, accountId }
  const orgId = nonEmptyString(record["orgId"])
  if (orgId) {
    return { ...input, orgId }
  }
  return input
}

const parseUpsertAccountInput = async (
  request: Request
): Promise<Response | UpsertAccountInput> => {
  const record = await parseJsonRecord(request)
  if (record instanceof Response) return record

  const id = nonEmptyString(record["id"])
  if (!id) return json({ error: "Missing id" }, { status: 400 })
  const orgId = nonEmptyString(record["orgId"])
  if (!orgId) return json({ error: "Missing orgId" }, { status: 400 })
  const kind = nonEmptyString(record["kind"])
  if (!kind || !accountKinds.has(kind as AccountKind)) {
    return json({ error: "Invalid kind" }, { status: 400 })
  }
  const label = nonEmptyString(record["label"])
  if (!label) return json({ error: "Missing label" }, { status: 400 })

  const input: UpsertAccountInput = {
    id,
    orgId,
    kind: kind as AccountKind,
    label,
    enabled: record["enabled"] === false ? false : true,
  }

  const credentials = record["credentials"]
  const totp = record["totp"]
  const modelQuotas = record["modelQuotas"]
  return {
    ...input,
    ...(typeof record["rateLimitRemaining"] === "number"
      ? { rateLimitRemaining: record["rateLimitRemaining"] }
      : {}),
    ...(credentials && typeof credentials === "object" && !Array.isArray(credentials)
      ? { credentials: credentials as AccountCredentials }
      : {}),
    ...(totp && typeof totp === "object" && !Array.isArray(totp)
      ? { totp: parseTotpConfig(totp as Record<string, unknown>) }
      : {}),
    ...(modelQuotas && typeof modelQuotas === "object" && !Array.isArray(modelQuotas)
      ? { modelQuotas: parseModelQuotas(modelQuotas as Record<string, unknown>) }
      : {}),
  }
}

const parseTenantAccountUploadInput = async (
  request: Request,
  tenantId: string
): Promise<Response | TenantAccountUploadInput> => {
  const record = await parseJsonRecord(request)
  if (record instanceof Response) return record

  const provider = nonEmptyString(record["provider"])
  if (!provider) return json({ error: "Missing provider" }, { status: 400 })
  const label = nonEmptyString(record["label"]) ?? defaultTenantAccountLabel(provider)
  if (!label) return json({ error: "Invalid provider" }, { status: 400 })

  try {
    const input = accountInputForTenantUpload(provider, label, tenantId, record)
    const validate = new URL(request.url).searchParams.get("validate") === "1"
    return { validate, account: input }
  } catch (error) {
    return json({ error: String((error as Error).message ?? error) }, { status: 400 })
  }
}

const accountInputForTenantUpload = (
  provider: string,
  label: string,
  tenantId: string,
  record: Record<string, unknown>
): UpsertAccountInput => {
  if (provider === "claude") {
    const claudeAiOauth = requireRecord(record["claudeAiOauth"], "claudeAiOauth")
    const accessToken = requireSecretString(claudeAiOauth["accessToken"], "claudeAiOauth.accessToken")
    const refreshToken = requireSecretString(claudeAiOauth["refreshToken"], "claudeAiOauth.refreshToken")
    const expiresAt = claudeAiOauth["expiresAt"]
    if (typeof expiresAt !== "number" || !Number.isFinite(expiresAt) || expiresAt <= 0) {
      throw new Error("Invalid claudeAiOauth.expiresAt")
    }
    return {
      id: randomAccountId("claude"),
      orgId: tenantId,
      kind: "anthropic_oauth",
      label,
      modelQuotas: anthropicBootstrapModelQuotas,
      credentials: {
        accessToken,
        refreshToken,
        expiresAt,
        ...optionalCredentialString(claudeAiOauth, "subscriptionType"),
        ...optionalCredentialString(claudeAiOauth, "rateLimitTier"),
        ...optionalCredentialString(claudeAiOauth, "tokenEndpoint"),
        ...optionalCredentialString(claudeAiOauth, "clientId"),
        ...optionalCredentialString(claudeAiOauth, "usageUrl"),
      },
    }
  }

  if (provider === "anthropic-apikey") {
    const apiKey = requireSecretString(record["apiKey"], "apiKey")
    if (!apiKey.startsWith("sk-ant-")) throw new Error("Invalid Anthropic API key")
    return {
      id: randomAccountId("anthropic"),
      orgId: tenantId,
      kind: "anthropic_apikey",
      label,
      modelQuotas: anthropicBootstrapModelQuotas,
      credentials: { apiKey },
    }
  }

  if (provider === "codex") {
    const tokens = requireRecord(record["tokens"], "tokens")
    const accessToken = requireSecretString(tokens["accessToken"], "tokens.accessToken")
    const refreshToken = requireSecretString(tokens["refreshToken"], "tokens.refreshToken")
    const idToken = requireSecretString(tokens["idToken"], "tokens.idToken")
    const accountId = requireSecretString(tokens["accountID"], "tokens.accountID")
    return {
      id: randomAccountId("codex"),
      orgId: tenantId,
      kind: "codex_oauth",
      label,
      modelQuotas: codexBootstrapModelQuotas,
      credentials: {
        accessToken,
        refreshToken,
        idToken,
        accountId,
        ...optionalCredentialString(tokens, "tokenEndpoint"),
        ...optionalCredentialString(tokens, "clientId"),
        ...optionalCredentialString(tokens, "usageUrl"),
      },
    }
  }

  if (provider === "openai-apikey") {
    const apiKey = requireSecretString(record["apiKey"], "apiKey")
    if (!apiKey.startsWith("sk-")) throw new Error("Invalid OpenAI API key")
    return {
      id: randomAccountId("openai"),
      orgId: tenantId,
      kind: "openai_apikey",
      label,
      credentials: { apiKey },
    }
  }

  throw new Error("Invalid provider")
}

const defaultTenantAccountLabel = (provider: string): string | null => {
  if (provider === "claude") return "Claude OAuth"
  if (provider === "anthropic-apikey") return "Anthropic API key"
  if (provider === "codex") return "Codex OAuth"
  if (provider === "openai-apikey") return "OpenAI API key"
  return null
}

const requireRecord = (value: unknown, name: string): Record<string, unknown> => {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`Missing ${name}`)
  }
  return value as Record<string, unknown>
}

const requireSecretString = (value: unknown, name: string): string => {
  const parsed = nonEmptyString(value)
  if (!parsed) throw new Error(`Missing ${name}`)
  return parsed
}

const optionalCredentialString = (
  record: Record<string, unknown>,
  name: string
): Record<string, string> => {
  const value = nonEmptyString(record[name])
  return value ? { [name]: value } : {}
}

const randomAccountId = (prefix: string): string => {
  const bytes = new Uint8Array(8)
  crypto.getRandomValues(bytes)
  const hex = [...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")
  return `${prefix}-${hex}`
}

const safeTenantAccount = (
  account: StoredAccount,
  validation?: "ok" | "failed"
): TenantAccountOutput => {
  const safe = safeGoAccount(account)
  return {
    id: account.id,
    provider: safe.provider,
    label: account.label,
    auth_mode: safe.auth_mode,
    created_at: new Date(account.createdAt).toISOString(),
    ...(safe.email ? { email: safe.email } : {}),
    ...(validation ? { validation } : {}),
  }
}

const accountMatchesUpstreamProvider = (
  account: StoredAccountContract,
  upstreamProvider: UpstreamProvider | undefined
): boolean => {
  if (!upstreamProvider) return true
  return providerForAccount(account.kind) === upstreamProvider
}

const parseModelProbeInput = async (
  request: Request
): Promise<Response | ModelProbeInput> => {
  const record = await parseJsonRecord(request)
  if (record instanceof Response) return record

  const accountId = nonEmptyString(record["accountId"])
  if (!accountId) return json({ error: "Missing accountId" }, { status: 400 })
  const model = nonEmptyString(record["model"])
  if (!model) return json({ error: "Missing model" }, { status: 400 })

  const input = {
    accountId,
    model,
    prompt:
      nonEmptyString(record["prompt"]) ??
      "Reply with exactly: subrouter-model-probe-ok",
  }
  const orgId = nonEmptyString(record["orgId"])
  if (orgId) return { ...input, orgId }
  return input
}

const parseTotpConfig = (record: Record<string, unknown>): TotpConfig => {
  const seed = nonEmptyString(record["seed"])
  if (!seed) throw new Error("Missing totp.seed")

  const digits = typeof record["digits"] === "number" ? record["digits"] : 6
  const period = typeof record["period"] === "number" ? record["period"] : 30
  const algorithm = nonEmptyString(record["algorithm"]) ?? "SHA-1"
  if (!["SHA-1", "SHA-256", "SHA-512"].includes(algorithm)) {
    throw new Error("Invalid totp.algorithm")
  }
  if (!Number.isInteger(digits) || digits < 6 || digits > 8) {
    throw new Error("Invalid totp.digits")
  }
  if (!Number.isInteger(period) || period < 15 || period > 120) {
    throw new Error("Invalid totp.period")
  }

  return {
    seed,
    digits,
    period,
    algorithm: algorithm as TotpAlgorithm,
  }
}

const parseModelQuotas = (
  record: Record<string, unknown>
): AccountModelQuotas => {
  const quotas: Record<string, AccountModelQuotas[string]> = {}
  for (const [rawKey, rawValue] of Object.entries(record)) {
    const key = rawKey.trim().toLowerCase()
    if (!key) continue

    if (typeof rawValue === "number") {
      quotas[key] = { remainingPercent: clampPercent(rawValue) }
      continue
    }

    if (!rawValue || typeof rawValue !== "object" || Array.isArray(rawValue)) {
      throw new Error(`Invalid quota for ${rawKey}`)
    }
    const value = rawValue as Record<string, unknown>
    const remaining = value["remainingPercent"] ?? value["remaining"]
    if (typeof remaining !== "number") {
      throw new Error(`Invalid quota remainingPercent for ${rawKey}`)
    }
    quotas[key] = {
      remainingPercent: clampPercent(remaining),
      ...(typeof value["resetsAt"] === "number"
        ? { resetsAt: value["resetsAt"] }
        : {}),
      ...(typeof value["protectedBelowPercent"] === "number"
        ? { protectedBelowPercent: clampPercent(value["protectedBelowPercent"]) }
        : {}),
    }
  }
  return quotas
}

const clampPercent = (value: number): number => {
  if (!Number.isFinite(value)) throw new Error("quota percent must be finite")
  return Math.max(0, Math.min(100, value))
}

const authorizeAdmin = (request: Request, env: Env): Response | null => {
  if (!env.ADMIN_TOKEN) {
    return json({ error: "ADMIN_TOKEN secret is not configured" }, { status: 500 })
  }
  const header = request.headers.get("authorization") ?? ""
  const bearer = header.startsWith("Bearer ") ? header.slice("Bearer ".length) : ""
  const token =
    bearer ||
    request.headers.get("x-admin-token") ||
    request.headers.get("x-subrouter-admin-token") ||
    ""
  if (!constantTimeStringEqual(token, env.ADMIN_TOKEN)) {
    return json({ error: "Unauthorized" }, { status: 401 })
  }
  return null
}

interface TenantAuth {
  readonly tenantId: string
  readonly legacy: boolean
}

const registryActor = (env: Env): DurableObjectStub<TenantRegistryDurableObject> =>
  env.TENANT_REGISTRY_DO.getByName("registry")

const registryTenantDOName = (tenantId: string): string => `tenant:${tenantId}`

const adminTenantDOName = (tenantId: string): string =>
  isReservedTenantId(tenantId) ? tenantId : registryTenantDOName(tenantId)

const tenantActor = (
  env: Env,
  tenant: TenantAuth
): DurableObjectStub<SubrouterDurableObject> =>
  env.SUBROUTER_DO.getByName(
    tenant.legacy ? tenant.tenantId : registryTenantDOName(tenant.tenantId)
  )

const authorizeTenant = async (
  request: Request,
  env: Env
): Promise<Response | TenantAuth> => {
  const header = request.headers.get("authorization") ?? ""
  const bearer = header.startsWith("Bearer ") ? header.slice("Bearer ".length) : ""
  const tenantKey = request.headers.get("x-subrouter-tenant-key") || bearer
  if (tenantKey.startsWith("srt_") || request.headers.has("x-subrouter-tenant-key")) {
    if (!isTenantKey(tenantKey)) {
      return json({ error: "Invalid tenant key" }, { status: 401 })
    }
    const resolved = await registryActor(env).resolveTenantKeySha256(
      await tenantKeySha256Hex(tenantKey)
    )
    if (!resolved) {
      return json({ error: "Invalid tenant key" }, { status: 401 })
    }
    if (resolved.revoked_at !== null) {
      return json({ error: "Tenant key revoked" }, { status: 403 })
    }
    return { tenantId: resolved.id, legacy: false }
  }

  const legacyToken =
    bearer || request.headers.get("x-subrouter-proxy-token") || ""
  if (
    env.ALLOW_LEGACY_PROXY_TOKEN === "1" &&
    env.PROXY_TOKEN &&
    constantTimeStringEqual(legacyToken, env.PROXY_TOKEN)
  ) {
    return { tenantId: "legacy", legacy: true }
  }

  return json({ error: "Tenant key required" }, { status: 401 })
}

const constantTimeStringEqual = (a: string, b: string): boolean => {
  const encoder = new TextEncoder()
  const aa = encoder.encode(a)
  const bb = encoder.encode(b)
  const length = Math.max(aa.length, bb.length)
  let diff = aa.length ^ bb.length
  for (let i = 0; i < length; i++) {
    diff |= (aa[i] ?? 0) ^ (bb[i] ?? 0)
  }
  return diff === 0
}

const rowToAccount = (row: AccountRow): StoredAccount => ({
  id: row.id,
  orgId: row.org_id,
  kind: row.kind as AccountKind,
  label: row.label,
  enabled: row.enabled === 1,
  ...(row.rate_limit_remaining !== null
    ? { rateLimitRemaining: row.rate_limit_remaining }
    : {}),
  hasCredentials: row.credentials_json !== null,
  hasTotp: row.totp_seed_base32 !== null,
  totpDigits: row.totp_digits,
  totpPeriod: row.totp_period,
  totpAlgorithm: row.totp_algorithm as TotpAlgorithm | null,
  createdAt: row.created_at,
  updatedAt: row.updated_at,
  source: "durable-object",
})

const quotaRowsToRecord = (
  rows: ReadonlyArray<ModelQuotaRow>
): AccountModelQuotas => {
  const quotas: Record<string, AccountModelQuotas[string]> = {}
  for (const row of rows) {
    quotas[row.quota_key] = {
      remainingPercent: row.remaining_percent,
      ...(row.resets_at !== null ? { resetsAt: row.resets_at } : {}),
      ...(row.protected_below_percent !== null
        ? { protectedBelowPercent: row.protected_below_percent }
        : {}),
    }
  }
  return quotas
}

interface MutableTranscriptSummary {
  agent_type: string
  session_id: string
  event_count: number
  total_bytes: number
  usage: MutableTranscriptUsage
  first_event_at?: string
  last_event_at?: string
  user?: string
  account?: string
  has_bodies: boolean
  size_bytes: number
}

interface MutableTranscriptUsage {
  requests: number
  input_tokens: number
  cached_input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  total_tokens: number
  payload_bytes: number
}

interface TranscriptUsageRecord {
  readonly timestamp: string
  readonly user: string
  readonly account: string
  readonly model: string
  readonly usage: MutableTranscriptUsage
}

interface TranscriptUsageGroup {
  readonly key: string
  readonly usage: MutableTranscriptUsage
}

interface TranscriptUsageBucket {
  readonly start: string
  readonly label: string
  readonly usage: MutableTranscriptUsage
}

interface TranscriptAnalytics {
  readonly totals: MutableTranscriptUsage
  readonly by_user: ReadonlyArray<TranscriptUsageGroup>
  readonly by_account: ReadonlyArray<TranscriptUsageGroup>
  readonly by_model: ReadonlyArray<TranscriptUsageGroup>
  readonly timeline: ReadonlyArray<TranscriptUsageBucket>
  readonly max_timeline_tokens: number
  readonly max_user_tokens: number
  readonly max_account_tokens: number
}

const emptyTranscriptUsage = (): MutableTranscriptUsage => ({
  requests: 0,
  input_tokens: 0,
  cached_input_tokens: 0,
  output_tokens: 0,
  reasoning_tokens: 0,
  total_tokens: 0,
  payload_bytes: 0,
})

const emptyTranscriptSummary = (
  agentType: string,
  sessionId: string
): MutableTranscriptSummary => ({
  agent_type: agentType,
  session_id: sessionId,
  event_count: 0,
  total_bytes: 0,
  usage: emptyTranscriptUsage(),
  has_bodies: false,
  size_bytes: 0,
})

const baseSessionId = (sessionId: string): string => {
  const index = sessionId.indexOf(":")
  return index === -1 ? sessionId : sessionId.slice(0, index)
}

const withTranscriptSession = (
  agentType: string,
  sessionId: string,
  payload: Record<string, unknown>
): Record<string, unknown> => {
  const normalizedAgent = normalizeAgentType(agentType) || "codex"
  const agentSessionId = baseSessionId(sessionId)
  return {
    ...payload,
    agent_type: normalizedAgent,
    session_id: sessionId,
    agent_session_id: agentSessionId,
    ...(normalizedAgent === "codex" ? { codex_session_id: agentSessionId } : {}),
  }
}

const applyTranscriptEvent = (
  summary: MutableTranscriptSummary,
  row: TranscriptEventRow
): void => {
  summary.event_count += 1
  summary.size_bytes += row.size_bytes
  if (!summary.first_event_at || row.timestamp < summary.first_event_at) {
    summary.first_event_at = row.timestamp
  }
  if (!summary.last_event_at || row.timestamp > summary.last_event_at) {
    summary.last_event_at = row.timestamp
  }

  const payload = parsePayload(row.payload_json)
  const user = stringPayload(payload["user"])
  if (user && !summary.user) summary.user = user
  const account = stringPayload(payload["account"])
  if (account && !summary.account) summary.account = account
  const bytes = numberPayload(payload["bytes"])
  if (bytes !== null) summary.total_bytes += bytes
  const encoded = stringPayload(payload["body_base64"])
  if (!encoded) return

  summary.has_bodies = true
  const body = base64ToString(encoded)
  if (body === null) return
  for (const record of usageRecordsFromBody(body, {
    timestamp: row.timestamp,
    user: summary.user ?? "",
    account: summary.account ?? "",
  })) {
    if (bytes !== null) record.usage.payload_bytes = bytes
    addTranscriptUsage(summary.usage, record.usage)
  }
}

const finalizeTranscriptSummary = (
  summary: MutableTranscriptSummary
): Record<string, unknown> => ({
  agent_type: summary.agent_type,
  session_id: summary.session_id,
  event_count: summary.event_count,
  total_bytes: summary.total_bytes,
  usage: summary.usage,
  ...(summary.first_event_at ? { first_event_at: summary.first_event_at } : {}),
  ...(summary.last_event_at ? { last_event_at: summary.last_event_at } : {}),
  ...(summary.user ? { user: summary.user } : {}),
  ...(summary.account ? { account: summary.account } : {}),
  has_bodies: summary.has_bodies,
  size_bytes: summary.size_bytes,
})

const transcriptEventFromRow = (
  row: TranscriptEventRow,
  raw: boolean
): Record<string, unknown> => {
  const payload = parsePayload(row.payload_json)
  if (!raw) {
    return {
      timestamp: row.timestamp,
      type: row.type,
      payload: sanitizeTranscriptPayload(payload),
    }
  }
  const encoded = stringPayload(payload["body_base64"])
  if (encoded) delete payload["body_base64"]
  const decoded = encoded ? base64ToString(encoded) : null
  return {
    timestamp: row.timestamp,
    type: row.type,
    payload,
    ...(decoded !== null ? { body_text: decoded } : encoded ? { body_base64: encoded } : {}),
  }
}

const parsePayload = (payloadJSON: string): Record<string, unknown> => {
  try {
    const payload = JSON.parse(payloadJSON) as unknown
    return payload && typeof payload === "object" && !Array.isArray(payload)
      ? (payload as Record<string, unknown>)
      : {}
  } catch {
    return {}
  }
}

const sanitizeTranscriptPayload = (
  payload: Record<string, unknown>
): Record<string, unknown> => {
  const out: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(payload)) {
    if (key === "body_base64") {
      out["body_base64_redacted"] = true
      continue
    }
    out[key] = value
  }
  return out
}

const usageRecordsFromBody = (
  body: string,
  context: {
    readonly timestamp: string
    readonly user: string
    readonly account: string
  }
): TranscriptUsageRecord[] => {
  const payloads = jsonPayloads(body)
  const out: TranscriptUsageRecord[] = []
  for (const payload of payloads) {
    const record = usageRecordFromPayload(payload, context)
    if (record) out.push(record)
  }
  return out
}

const jsonPayloads = (body: string): Record<string, unknown>[] => {
  const parsed = parseJsonRecordValue(body)
  if (parsed) return [parsed]
  const out: Record<string, unknown>[] = []
  for (const rawLine of body.split(/\r?\n/)) {
    const line = rawLine.trim().replace(/^data:\s*/, "").trim()
    if (!line || line === "[DONE]") continue
    const item = parseJsonRecordValue(line)
    if (item) out.push(item)
  }
  return out
}

const parseJsonRecordValue = (text: string): Record<string, unknown> | null => {
  try {
    const parsed = JSON.parse(text) as unknown
    return parsed && typeof parsed === "object" && !Array.isArray(parsed)
      ? (parsed as Record<string, unknown>)
      : null
  } catch {
    return null
  }
}

const usageRecordFromPayload = (
  payload: Record<string, unknown>,
  context: {
    readonly timestamp: string
    readonly user: string
    readonly account: string
  }
): TranscriptUsageRecord | null => {
  const response = recordPayload(payload["response"])
  const container = response ?? payload
  const usage = recordPayload(container["usage"])
  if (!usage) return null
  const input = numberFieldPayload(usage, ["input_tokens", "prompt_tokens"])
  const output = numberFieldPayload(usage, ["output_tokens", "completion_tokens"])
  let total = numberFieldPayload(usage, ["total_tokens"])
  if (total === 0) total = input + output
  if (total === 0 && input === 0 && output === 0) return null
  return {
    timestamp: context.timestamp,
    user: context.user,
    account: context.account,
    model: stringPayload(container["model"]) ?? "",
    usage: {
    requests: 1,
    input_tokens: input,
    cached_input_tokens: numberFieldPayload(recordPayload(usage["input_tokens_details"]) ?? {}, ["cached_tokens"]),
    output_tokens: output,
    reasoning_tokens: numberFieldPayload(recordPayload(usage["output_tokens_details"]) ?? {}, ["reasoning_tokens"]),
    total_tokens: total,
    payload_bytes: 0,
    },
  }
}

const usageRecordsFromRows = (
  rows: ReadonlyArray<TranscriptEventRow>
): TranscriptUsageRecord[] => {
  const records: TranscriptUsageRecord[] = []
  const contexts = new Map<string, { user: string; account: string }>()
  for (const row of rows) {
    const key = `${row.agent_type}\u0000${row.session_id}`
    const context = contexts.get(key) ?? { user: "", account: "" }
    const payload = parsePayload(row.payload_json)
    const user = stringPayload(payload["user"])
    if (user) context.user = user
    const account = stringPayload(payload["account"])
    if (account) context.account = account
    contexts.set(key, context)
    const encoded = stringPayload(payload["body_base64"])
    if (!encoded) continue
    const body = base64ToString(encoded)
    if (body === null) continue
    const bytes = numberPayload(payload["bytes"])
    for (const record of usageRecordsFromBody(body, {
      timestamp: row.timestamp,
      user: context.user,
      account: context.account,
    })) {
      if (bytes !== null) record.usage.payload_bytes = bytes
      records.push(record)
    }
  }
  return records
}

const buildTranscriptAnalytics = (
  records: ReadonlyArray<TranscriptUsageRecord>
): TranscriptAnalytics => {
  const totals = emptyTranscriptUsage()
  const byUser = new Map<string, MutableTranscriptUsage>()
  const byAccount = new Map<string, MutableTranscriptUsage>()
  const byModel = new Map<string, MutableTranscriptUsage>()
  let first = 0
  let last = 0

  for (const record of records) {
    addTranscriptUsage(totals, record.usage)
    addUsageToGroup(byUser, orUnknown(record.user), record.usage)
    addUsageToGroup(byAccount, orUnknown(record.account), record.usage)
    addUsageToGroup(byModel, orUnknown(record.model), record.usage)
    const time = Date.parse(record.timestamp)
    if (Number.isFinite(time)) {
      if (first === 0 || time < first) first = time
      if (last === 0 || time > last) last = time
    }
  }

  const by_user = sortedUsageGroups(byUser)
  const by_account = sortedUsageGroups(byAccount)
  const by_model = sortedUsageGroups(byModel)
  const timeline = buildTranscriptTimeline(records, first, last)

  return {
    totals,
    by_user,
    by_account,
    by_model,
    timeline,
    max_timeline_tokens: maxUsageTokens(timeline.map((bucket) => bucket.usage)),
    max_user_tokens: maxUsageTokens(by_user.map((group) => group.usage)),
    max_account_tokens: maxUsageTokens(by_account.map((group) => group.usage)),
  }
}

const addUsageToGroup = (
  groups: Map<string, MutableTranscriptUsage>,
  key: string,
  usage: MutableTranscriptUsage
): void => {
  const current = groups.get(key) ?? emptyTranscriptUsage()
  addTranscriptUsage(current, usage)
  groups.set(key, current)
}

const sortedUsageGroups = (
  groups: Map<string, MutableTranscriptUsage>
): TranscriptUsageGroup[] =>
  [...groups.entries()]
    .map(([key, usage]) => ({ key, usage }))
    .sort((left, right) => {
      if (left.usage.total_tokens === right.usage.total_tokens) {
        return left.key.localeCompare(right.key)
      }
      return right.usage.total_tokens - left.usage.total_tokens
    })

const buildTranscriptTimeline = (
  records: ReadonlyArray<TranscriptUsageRecord>,
  first: number,
  last: number
): TranscriptUsageBucket[] => {
  if (first === 0 || last === 0) return []
  const size = transcriptBucketSize(last - first)
  const start = Math.floor(first / size) * size
  const buckets = new Map<number, MutableTranscriptUsage>()
  for (const record of records) {
    const time = Date.parse(record.timestamp)
    if (!Number.isFinite(time)) continue
    const bucketStart = Math.floor(time / size) * size
    const usage = buckets.get(bucketStart) ?? emptyTranscriptUsage()
    addTranscriptUsage(usage, record.usage)
    buckets.set(bucketStart, usage)
  }
  const out: TranscriptUsageBucket[] = []
  for (let cursor = start; cursor <= last; cursor += size) {
    out.push({
      start: new Date(cursor).toISOString(),
      label: transcriptBucketLabel(cursor, size),
      usage: buckets.get(cursor) ?? emptyTranscriptUsage(),
    })
  }
  return out
}

const transcriptBucketSize = (spanMs: number): number => {
  if (spanMs <= 2 * 60 * 60 * 1000) return 60 * 1000
  if (spanMs <= 48 * 60 * 60 * 1000) return 60 * 60 * 1000
  return 24 * 60 * 60 * 1000
}

const transcriptBucketLabel = (time: number, size: number): string => {
  const date = new Date(time)
  if (size < 60 * 60 * 1000) {
    return `${pad2(date.getUTCHours())}:${pad2(date.getUTCMinutes())}`
  }
  if (size < 24 * 60 * 60 * 1000) {
    return `${date.toISOString().slice(0, 10)} ${pad2(date.getUTCHours())}:00`
  }
  return date.toISOString().slice(0, 10)
}

const pad2 = (value: number): string => String(value).padStart(2, "0")

const maxUsageTokens = (usages: ReadonlyArray<MutableTranscriptUsage>): number =>
  usages.reduce((max, usage) => Math.max(max, usage.total_tokens), 0)

const orUnknown = (value: string): string => (value.trim() ? value : "(unknown)")

const addTranscriptUsage = (
  target: MutableTranscriptUsage,
  usage: MutableTranscriptUsage
): void => {
  target.requests += usage.requests
  target.input_tokens += usage.input_tokens
  target.cached_input_tokens += usage.cached_input_tokens
  target.output_tokens += usage.output_tokens
  target.reasoning_tokens += usage.reasoning_tokens
  target.total_tokens += usage.total_tokens
  target.payload_bytes += usage.payload_bytes
}

const recordPayload = (value: unknown): Record<string, unknown> | null =>
  value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null

const stringPayload = (value: unknown): string | null =>
  typeof value === "string" && value.length > 0 ? value : null

const numberPayload = (value: unknown): number | null =>
  typeof value === "number" && Number.isFinite(value) ? value : null

const numberFieldPayload = (
  values: Record<string, unknown>,
  names: ReadonlyArray<string>
): number => {
  for (const name of names) {
    const value = numberPayload(values[name])
    if (value !== null) return value
  }
  return 0
}

const base64ToString = (value: string): string | null => {
  try {
    return atob(value)
  } catch {
    return null
  }
}

const decodeBase32 = (input: string): Uint8Array => {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
  const normalized = input.toUpperCase().replaceAll(/[\s=-]/g, "")
  let bits = 0
  let value = 0
  const bytes: number[] = []

  for (const char of normalized) {
    const index = alphabet.indexOf(char)
    if (index === -1) {
      throw new Error("TOTP seed is not valid base32")
    }
    value = (value << 5) | index
    bits += 5
    if (bits >= 8) {
      bytes.push((value >>> (bits - 8)) & 0xff)
      bits -= 8
    }
  }

  return new Uint8Array(bytes)
}

const generateTotp = async (
  seedBase32: string,
  nowMs: number,
  digits: number,
  period: number,
  algorithm: TotpAlgorithm
) => {
  const secret = decodeBase32(seedBase32)
  const keyData = new ArrayBuffer(secret.byteLength)
  new Uint8Array(keyData).set(secret)
  const key = await crypto.subtle.importKey(
    "raw",
    keyData,
    { name: "HMAC", hash: algorithm },
    false,
    ["sign"]
  )
  const counter = Math.floor(nowMs / 1000 / period)
  const buffer = new ArrayBuffer(8)
  const view = new DataView(buffer)
  view.setUint32(4, counter)

  const signature = new Uint8Array(await crypto.subtle.sign("HMAC", key, buffer))
  const offset = signature[signature.length - 1]! & 0x0f
  const binary =
    ((signature[offset]! & 0x7f) << 24) |
    (signature[offset + 1]! << 16) |
    (signature[offset + 2]! << 8) |
    signature[offset + 3]!
  const modulo = 10 ** digits
  const code = String(binary % modulo).padStart(digits, "0")

  return {
    code,
    period,
    digits,
    algorithm,
    secondsRemaining: period - (Math.floor(nowMs / 1000) % period),
  }
}

export class SubrouterDurableObject extends DurableObject<Env> {
  private readonly refreshInFlight = new Map<string, Promise<AccountRow | null>>()

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env)
    this.ctx.setWebSocketAutoResponse(
      new WebSocketRequestResponsePair("ping", "pong")
    )

    this.ctx.storage.sql.exec(`
      CREATE TABLE IF NOT EXISTS accounts(
        id TEXT PRIMARY KEY,
        org_id TEXT NOT NULL,
        kind TEXT NOT NULL,
        label TEXT NOT NULL,
        enabled INTEGER NOT NULL DEFAULT 1,
        rate_limit_remaining INTEGER,
        credentials_json TEXT,
        totp_seed_base32 TEXT,
        totp_digits INTEGER,
        totp_period INTEGER,
        totp_algorithm TEXT,
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL
      );
      CREATE INDEX IF NOT EXISTS accounts_org_enabled_idx
        ON accounts(org_id, enabled);
      CREATE TABLE IF NOT EXISTS subrouter_status(
        id INTEGER PRIMARY KEY CHECK (id = 1),
        route_count INTEGER NOT NULL DEFAULT 0,
        usage_count INTEGER NOT NULL DEFAULT 0,
        last_routed_at INTEGER,
        last_account_id TEXT,
        last_session_id TEXT,
        cursor INTEGER NOT NULL DEFAULT 0
      );
      CREATE TABLE IF NOT EXISTS sticky_sessions(
        org_id TEXT NOT NULL,
        session_id TEXT NOT NULL,
        account_id TEXT NOT NULL,
        PRIMARY KEY (org_id, session_id)
      );
      CREATE TABLE IF NOT EXISTS sticky_session_routes(
        org_id TEXT NOT NULL,
        quota_key TEXT NOT NULL,
        session_id TEXT NOT NULL,
        account_id TEXT NOT NULL,
        PRIMARY KEY (org_id, quota_key, session_id)
      );
      CREATE TABLE IF NOT EXISTS session_assignments(
        org_id TEXT NOT NULL,
        agent_type TEXT NOT NULL,
        session_id TEXT NOT NULL,
        quota_key TEXT NOT NULL,
        account_id TEXT NOT NULL,
        user_email TEXT,
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL,
        PRIMARY KEY (org_id, agent_type, quota_key, session_id)
      );
      CREATE TABLE IF NOT EXISTS account_usage(
        account_id TEXT PRIMARY KEY,
        last_used_at INTEGER NOT NULL
      );
      CREATE TABLE IF NOT EXISTS account_model_quotas(
        account_id TEXT NOT NULL,
        quota_key TEXT NOT NULL,
        remaining_percent REAL NOT NULL,
        resets_at INTEGER,
        protected_below_percent REAL,
        PRIMARY KEY (account_id, quota_key)
      );
      CREATE TABLE IF NOT EXISTS transcript_events(
        event_id INTEGER PRIMARY KEY AUTOINCREMENT,
        org_id TEXT NOT NULL,
        agent_type TEXT NOT NULL,
        session_id TEXT NOT NULL,
        timestamp TEXT NOT NULL,
        type TEXT NOT NULL,
        payload_json TEXT NOT NULL,
        size_bytes INTEGER NOT NULL
      );
      CREATE INDEX IF NOT EXISTS transcript_events_session_idx
        ON transcript_events(org_id, agent_type, session_id, event_id);
      CREATE INDEX IF NOT EXISTS transcript_events_org_time_idx
        ON transcript_events(org_id, timestamp);
      CREATE TABLE IF NOT EXISTS lifecycle(
        id INTEGER PRIMARY KEY CHECK (id = 1),
        draining INTEGER NOT NULL DEFAULT 0,
        started_at INTEGER NOT NULL
      );
      INSERT OR IGNORE INTO subrouter_status(id) VALUES (1);
      INSERT OR IGNORE INTO lifecycle(id, started_at) VALUES (1, ${Date.now()});
    `)

    this.ctx.blockConcurrencyWhile(async () => {
      await this.scheduleNextRefreshAlarm()
    })
  }

  override async alarm(): Promise<void> {
    await this.refreshOAuthAccounts(false)
    await this.scheduleNextRefreshAlarm()
  }

  override async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url)
    if (url.pathname !== "/ws") {
      return new Response("Not Found", { status: 404 })
    }
    if (request.headers.get("Upgrade")?.toLowerCase() !== "websocket") {
      return json({ error: "Expected WebSocket Upgrade request" }, { status: 426 })
    }

    const orgId = this.resolveOrgId(url.searchParams.get("orgId") ?? undefined)
    const pair = new WebSocketPair()
    const [client, server] = Object.values(pair) as [WebSocket, WebSocket]
    server.serializeAttachment({
      id: crypto.randomUUID(),
      orgId,
      connectedAt: Date.now(),
      lastMessageAt: null,
      messageCount: 0,
    } satisfies WebSocketAttachment)
    this.ctx.acceptWebSocket(server, [orgId])
    return new Response(null, { status: 101, webSocket: client })
  }

  override async webSocketMessage(
    webSocket: WebSocket,
    message: string | ArrayBuffer
  ): Promise<void> {
    if (typeof message !== "string") {
      webSocket.send(
        JSON.stringify({
          type: "error",
          ok: false,
          error: "Binary WebSocket messages are not supported",
        } satisfies WebSocketServerMessage)
      )
      return
    }

    const attachment = this.recordWebSocketMessage(webSocket)
    try {
      const parsed = JSON.parse(message)
      const messages = normalizeWebSocketMessage(parsed)
      const replies: WebSocketServerMessage[] = []
      for (const entry of messages) {
        replies.push(await this.handleWebSocketMessage(attachment, entry))
      }
      if (replies.length === 1) {
        webSocket.send(JSON.stringify(replies[0]))
      } else {
        webSocket.send(
          JSON.stringify({
            type: "batch",
            ok: true,
            connectionId: attachment.id,
            messages: replies,
          } satisfies WebSocketServerMessage)
        )
      }
    } catch (error) {
      webSocket.send(
        JSON.stringify({
          type: "error",
          ok: false,
          connectionId: attachment.id,
          error: String((error as Error).message ?? error),
        } satisfies WebSocketServerMessage)
      )
    }
  }

  override async webSocketClose(
    _webSocket: WebSocket,
    _code: number,
    _reason: string,
    _wasClean: boolean
  ): Promise<void> {
    // Durable Object hibernation resumes connections from attachments; no timer
    // cleanup is needed because closed sockets disappear from getWebSockets().
  }

  override async webSocketError(
    _webSocket: WebSocket,
    error: unknown
  ): Promise<void> {
    console.error("subrouter websocket error", error)
  }

  async webSocketStats(): Promise<WebSocketStatsOutput> {
    const connections = this.ctx
      .getWebSockets()
      .map((webSocket) => webSocketAttachment(webSocket))
      .sort((a, b) => a.connectedAt - b.connectedAt)
    return {
      connectionCount: connections.length,
      connections,
    }
  }

  async route(input: RouteInput): Promise<RouteOutput> {
    const orgId = this.resolveOrgId(input.orgId)
    const agentType = this.resolveAgentType(input.agentType)
    const sessionId = stickySessionId(
      agentType,
      this.requireNonEmpty(input.sessionId, "sessionId")
    )
    const quotaKey = this.resolveQuotaKey(input)
    const upstreamProvider = this.resolveUpstreamProvider(input)
    const routedAt = Date.now()
    const sql = this.ctx.storage.sql

    const sticky = sql
      .exec<SessionAssignmentRow>(
        `SELECT account_id FROM session_assignments
         WHERE org_id = ? AND agent_type = ? AND quota_key = ? AND session_id = ?`,
        orgId,
        agentType,
        quotaKey,
        sessionId
      )
      .toArray()[0]
    const stickyAccount = sticky
      ? this.getEligibleAccount(sql, orgId, sticky.account_id, quotaKey, true, upstreamProvider)
      : null
    if (stickyAccount) {
      this.recordRouteMetadata(sql, routedAt, stickyAccount.id, sessionId)
      return {
        account: this.accountWithLastUsed(sql, stickyAccount),
        routedAt,
        quotaKey,
      }
    }

    const preferredAccount = input.preferAccountId
      ? this.getEligibleAccount(sql, orgId, input.preferAccountId, quotaKey, true, upstreamProvider)
      : null
    if (preferredAccount) {
      this.rememberSticky(sql, orgId, agentType, quotaKey, sessionId, preferredAccount.id, input.userEmail)
      this.recordUse(sql, preferredAccount.id)
      this.recordRouteMetadata(sql, routedAt, preferredAccount.id, sessionId)
      return {
        account: this.accountWithLastUsed(sql, preferredAccount),
        routedAt,
        quotaKey,
      }
    }

    const eligible = this.listEligibleAccounts(sql, orgId, quotaKey, upstreamProvider)
    if (eligible.length === 0) {
      const providerLabel = upstreamProvider ? `${upstreamProvider} ` : ""
      throw new Error(`no eligible ${providerLabel}${quotaKey} subrouter account for org ${orgId}`)
    }

    const status = this.statusRow(sql)
    const account = eligible[status.cursor % eligible.length]!
    sql.exec("UPDATE subrouter_status SET cursor = cursor + 1 WHERE id = 1")
    this.rememberSticky(sql, orgId, agentType, quotaKey, sessionId, account.id, input.userEmail)
    this.recordUse(sql, account.id)
    this.recordRouteMetadata(sql, routedAt, account.id, sessionId)
    return { account: this.accountWithLastUsed(sql, account), routedAt, quotaKey }
  }

  async routeForProxy(
    input: RouteInput
  ): Promise<RouteOutput & { readonly account: StoredAccountContract }> {
    const routed = await this.route(input)
    const row = await this.refreshAccountIfExpired(
      routed.account.orgId,
      routed.account.id,
      false
    )
    if (!row) throw new Error("selected account disappeared")
    const credentials = row.credentials_json
      ? (JSON.parse(row.credentials_json) as AccountCredentials)
      : undefined
    return {
      ...routed,
      account: {
        ...routed.account,
        ...(credentials ? { credentials } : {}),
      },
    }
  }

  async recordUsage(input: UsageInput): Promise<{ ok: true }> {
    const orgId = this.resolveOrgId(input.orgId)
    const sessionId = this.requireNonEmpty(input.sessionId, "sessionId")
    const accountId = this.requireNonEmpty(input.accountId, "accountId")
    const sql = this.ctx.storage.sql
    if (!this.getAccountRow(sql, orgId, accountId, false)) {
      throw new Error("account does not belong to org")
    }

    this.recordUse(sql, accountId)
    sql.exec(
      `UPDATE subrouter_status
       SET usage_count = usage_count + 1,
           last_account_id = ?,
           last_session_id = ?
       WHERE id = 1`,
      accountId,
      sessionId
    )
    return { ok: true }
  }

  async status(): Promise<StatusOutput> {
    const sql = this.ctx.storage.sql
    const row = this.statusRow(sql)
    return {
      routeCount: row.route_count,
      usageCount: row.usage_count,
      accountCount: sql.exec<CountRow>("SELECT COUNT(*) AS count FROM accounts").one()
        .count,
      sessionCount: sql
        .exec<CountRow>("SELECT COUNT(*) AS count FROM session_assignments")
        .one().count,
      lastRoutedAt: row.last_routed_at,
      lastAccountId: row.last_account_id,
      lastSessionId: row.last_session_id,
      draining: this.isDraining(),
    }
  }

  async ready(): Promise<{ ok: boolean; draining: boolean }> {
    const draining = this.isDraining()
    return { ok: !draining, draining }
  }

  async drain(): Promise<{ ok: true; draining: true }> {
    this.ctx.storage.sql.exec("UPDATE lifecycle SET draining = 1 WHERE id = 1")
    return { ok: true, draining: true }
  }

  async listSessions(): Promise<ReadonlyArray<Record<string, unknown>>> {
    return this.ctx.storage.sql
      .exec<SessionAssignmentRow>(
        `SELECT * FROM session_assignments
         ORDER BY updated_at DESC, session_id`
      )
      .toArray()
      .map((row) => ({
        org_id: row.org_id,
        agent_type: row.agent_type,
        session_id: row.session_id,
        quota_key: row.quota_key,
        account_id: row.account_id,
        ...(row.user_email ? { user_email: row.user_email } : {}),
        created_at: new Date(row.created_at).toISOString(),
        updated_at: new Date(row.updated_at).toISOString(),
      }))
  }

  async upsertAccount(input: UpsertAccountInput): Promise<{ account: StoredAccount }> {
    const sql = this.ctx.storage.sql
    const now = Date.now()
    sql.exec(
      `INSERT INTO accounts(
        id,
        org_id,
        kind,
        label,
        enabled,
        rate_limit_remaining,
        credentials_json,
        totp_seed_base32,
        totp_digits,
        totp_period,
        totp_algorithm,
        created_at,
        updated_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(id) DO UPDATE SET
        org_id = excluded.org_id,
        kind = excluded.kind,
        label = excluded.label,
        enabled = excluded.enabled,
        rate_limit_remaining = excluded.rate_limit_remaining,
        credentials_json = COALESCE(excluded.credentials_json, accounts.credentials_json),
        totp_seed_base32 = COALESCE(excluded.totp_seed_base32, accounts.totp_seed_base32),
        totp_digits = COALESCE(excluded.totp_digits, accounts.totp_digits),
        totp_period = COALESCE(excluded.totp_period, accounts.totp_period),
        totp_algorithm = COALESCE(excluded.totp_algorithm, accounts.totp_algorithm),
        updated_at = excluded.updated_at`,
      input.id,
      input.orgId,
      input.kind,
      input.label,
      input.enabled === false ? 0 : 1,
      input.rateLimitRemaining ?? null,
      input.credentials ? JSON.stringify(input.credentials) : null,
      input.totp?.seed ?? null,
      input.totp?.digits ?? null,
      input.totp?.period ?? null,
      input.totp?.algorithm ?? null,
      now,
      now
    )
    this.upsertModelQuotas(input.id, input.modelQuotas)
    const account = this.getAccount(sql, input.orgId, input.id, false)
    if (!account) throw new Error("account upsert failed")
    await this.scheduleNextRefreshAlarm()
    return { account }
  }

  async listAccounts(orgId: string): Promise<{ accounts: ReadonlyArray<StoredAccount> }> {
    return {
      accounts: this.ctx.storage.sql
        .exec<AccountRow>(
          `SELECT * FROM accounts
           WHERE org_id = ?
           ORDER BY id`,
          orgId
        )
        .toArray()
        .map((row) => this.accountFromRow(row)),
    }
  }

  async deleteAccount(input: { readonly orgId?: string; readonly accountId: string }): Promise<{ ok: true }> {
    const orgId = this.resolveOrgId(input.orgId)
    const accountId = this.requireNonEmpty(input.accountId, "accountId")
    const row = this.getAccountRow(this.ctx.storage.sql, orgId, accountId, false)
    if (!row) throw new Error("account not found")
    const sql = this.ctx.storage.sql
    sql.exec("DELETE FROM accounts WHERE id = ? AND org_id = ?", accountId, orgId)
    sql.exec("DELETE FROM account_model_quotas WHERE account_id = ?", accountId)
    sql.exec("DELETE FROM account_usage WHERE account_id = ?", accountId)
    sql.exec(
      "DELETE FROM session_assignments WHERE org_id = ? AND account_id = ?",
      orgId,
      accountId
    )
    sql.exec(
      "DELETE FROM sticky_sessions WHERE org_id = ? AND account_id = ?",
      orgId,
      accountId
    )
    sql.exec(
      "DELETE FROM sticky_session_routes WHERE org_id = ? AND account_id = ?",
      orgId,
      accountId
    )
    await this.scheduleNextRefreshAlarm()
    return { ok: true }
  }

  async accountStatuses(orgId: string, forceRefresh: boolean): Promise<ReadonlyArray<OAuthRefreshStatus>> {
    const rows = this.ctx.storage.sql
      .exec<AccountRow>(
        "SELECT * FROM accounts WHERE org_id = ? ORDER BY id",
        this.resolveOrgId(orgId)
      )
      .toArray()
    const statuses: OAuthRefreshStatus[] = []
    for (const row of rows) {
      statuses.push(await this.refreshStatusForRow(row, forceRefresh))
    }
    await this.scheduleNextRefreshAlarm()
    return statuses
  }

  async usageStatuses(orgId: string): Promise<ReadonlyArray<Record<string, unknown>>> {
    const resolvedOrgId = this.resolveOrgId(orgId)
    const rows = this.ctx.storage.sql
      .exec<AccountRow>(
        "SELECT * FROM accounts WHERE org_id = ? ORDER BY id",
        resolvedOrgId
      )
      .toArray()
    const out: Record<string, unknown>[] = []
    for (const row of rows) {
      const auth = await this.refreshStatusForRow(row, false)
      const account = this.accountFromRow(row)
      const fallback = usageStatus(account)
      const entry: Record<string, unknown> = { ...fallback, ...auth }
      const refreshedRow = this.getAccountRow(this.ctx.storage.sql, resolvedOrgId, row.id, false)
      const credentials = refreshedRow?.credentials_json
        ? (JSON.parse(refreshedRow.credentials_json) as AccountCredentials)
        : undefined
      if (!auth.error && credentials && isOAuthKind(row.kind as AccountKind)) {
        try {
          Object.assign(
            entry,
            await fetchProviderUsage(row.kind as AccountKind, credentials)
          )
        } catch (error) {
          entry.error = String((error as Error).message ?? error)
        }
      }
      out.push(entry)
    }
    await this.scheduleNextRefreshAlarm()
    return out
  }

  async refreshOAuthAccounts(forceRefresh: boolean): Promise<ReadonlyArray<OAuthRefreshStatus>> {
    const rows = this.ctx.storage.sql
      .exec<AccountRow>(
        `SELECT * FROM accounts
         WHERE kind IN ('codex_oauth', 'anthropic_oauth')
         ORDER BY org_id, id`
      )
      .toArray()
    const statuses: OAuthRefreshStatus[] = []
    for (const row of rows) {
      statuses.push(await this.refreshStatusForRow(row, forceRefresh))
    }
    return statuses
  }

  async totpCode(input: { orgId?: string; accountId: string }) {
    const orgId = this.resolveOrgId(input.orgId)
    const accountId = this.requireNonEmpty(input.accountId, "accountId")
    const row = this.getAccountRow(this.ctx.storage.sql, orgId, accountId, false)
    if (!row) throw new Error("account not found")
    if (!row.totp_seed_base32) throw new Error("account has no totp seed")

    return generateTotp(
      row.totp_seed_base32,
      Date.now(),
      row.totp_digits ?? 6,
      row.totp_period ?? 30,
      (row.totp_algorithm as TotpAlgorithm | null) ?? "SHA-1"
    )
  }

  async modelProbe(input: ModelProbeInput) {
    const orgId = this.resolveOrgId(input.orgId)
    const accountId = this.requireNonEmpty(input.accountId, "accountId")
    const row = this.getAccountRow(this.ctx.storage.sql, orgId, accountId, false)
    if (!row) throw new Error("account not found")
    if (!row.credentials_json) {
      throw new Error("account has no credentials")
    }

    const credentials = JSON.parse(row.credentials_json) as AccountCredentials
    const apiKey = credentials.apiKey ?? credentials.accessToken
    if (!apiKey) {
      throw new Error("account credentials need apiKey or accessToken")
    }
    const baseUrl = credentials.baseUrl ?? "https://api.openai.com/v1"
    const response = await fetch(`${baseUrl.replace(/\/$/, "")}/responses`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${apiKey}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        model: input.model,
        input: input.prompt,
        max_output_tokens: 32,
      }),
    })
    const body = await response.text()
    return {
      ok: response.ok,
      status: response.status,
      model: input.model,
      body: parseJsonOrText(body),
    }
  }

  async recordTranscriptMeta(input: TranscriptEventInput): Promise<{ ok: true }> {
    const agentType = normalizeAgentType(input.agentType) || "codex"
    const sessionId = this.requireNonEmpty(input.sessionId, "sessionId")
    this.recordTranscriptEvent({
      ...input,
      agentType,
      sessionId,
      type: input.type || "subrouter_meta",
      payload: withTranscriptSession(agentType, sessionId, input.payload),
    })
    return { ok: true }
  }

  async recordTranscriptBody(input: TranscriptBodyInput): Promise<{ ok: true }> {
    const agentType = normalizeAgentType(input.agentType) || "codex"
    const sessionId = this.requireNonEmpty(input.sessionId, "sessionId")
    this.recordTranscriptEvent({
      orgId: input.orgId,
      agentType,
      sessionId,
      type: input.eventType,
      payload: withTranscriptSession(agentType, sessionId, {
        ...(input.payload ?? {}),
        direction: input.direction,
        bytes: input.bytes,
        sha256: input.sha256,
        body_base64: input.bodyBase64,
      }),
    })
    return { ok: true }
  }

  async listTranscriptSummaries(orgId?: string): Promise<ReadonlyArray<Record<string, unknown>>> {
    const rows = this.listTranscriptRows(orgId)
    const summaries = new Map<string, MutableTranscriptSummary>()
    for (const row of rows) {
      const key = `${row.agent_type}\u0000${baseSessionId(row.session_id)}`
      let summary = summaries.get(key)
      if (!summary) {
        summary = emptyTranscriptSummary(row.agent_type, baseSessionId(row.session_id))
        summaries.set(key, summary)
      }
      applyTranscriptEvent(summary, row)
    }
    return [...summaries.values()]
      .map((summary) => finalizeTranscriptSummary(summary))
      .sort((left, right) => {
        const leftLast = String(left["last_event_at"] ?? "")
        const rightLast = String(right["last_event_at"] ?? "")
        if (leftLast === rightLast) {
          return String(left["session_id"] ?? "").localeCompare(String(right["session_id"] ?? ""))
        }
        return rightLast.localeCompare(leftLast)
      })
  }

  async transcriptDashboardData(orgId?: string): Promise<TranscriptDashboardData> {
    const rows = this.listTranscriptRows(orgId)
    const summaries = new Map<string, MutableTranscriptSummary>()
    for (const row of rows) {
      const key = `${row.agent_type}\u0000${baseSessionId(row.session_id)}`
      let summary = summaries.get(key)
      if (!summary) {
        summary = emptyTranscriptSummary(row.agent_type, baseSessionId(row.session_id))
        summaries.set(key, summary)
      }
      applyTranscriptEvent(summary, row)
    }
    return {
      summaries: [...summaries.values()]
        .map((summary) => finalizeTranscriptSummary(summary))
        .sort((left, right) => {
          const leftLast = String(left["last_event_at"] ?? "")
          const rightLast = String(right["last_event_at"] ?? "")
          if (leftLast === rightLast) {
            return String(left["session_id"] ?? "").localeCompare(String(right["session_id"] ?? ""))
          }
          return rightLast.localeCompare(leftLast)
        }),
      analytics: buildTranscriptAnalytics(usageRecordsFromRows(rows)),
    }
  }

  async readTranscriptSession(input: TranscriptReadInput): Promise<Record<string, unknown>> {
    const orgId = this.resolveOrgId(input.orgId)
    const agentType = normalizeAgentType(input.agentType) || "codex"
    const sessionId = baseSessionId(this.requireNonEmpty(input.sessionId, "sessionId"))
    const rows = this.ctx.storage.sql
      .exec<TranscriptEventRow>(
        `SELECT * FROM transcript_events
         WHERE org_id = ? AND agent_type = ? AND session_id = ?
         ORDER BY event_id`,
        orgId,
        agentType,
        sessionId
      )
      .toArray()
    if (rows.length === 0) throw new Error("transcript not found")
    return {
      agent_type: agentType,
      session_id: sessionId,
      events: rows.map((row) => transcriptEventFromRow(row, input.raw)),
    }
  }

  private resolveOrgId(inputOrgId: string | undefined): string {
    const orgId = inputOrgId ?? "demo-org"
    return this.requireNonEmpty(orgId, "orgId")
  }

  private resolveAgentType(inputAgentType: AgentType | undefined): string {
    return this.requireNonEmpty(inputAgentType || "codex", "agentType")
  }

  private resolveQuotaKey(input: { model?: string; quotaKey?: string }): string {
    const explicit = input.quotaKey?.trim().toLowerCase()
    if (explicit) return explicit
    return quotaKeyForModel(input.model)
  }

  private resolveUpstreamProvider(input: { upstreamProvider?: UpstreamProvider; agentType?: AgentType }): UpstreamProvider | undefined {
    if (input.upstreamProvider === "claude" || input.upstreamProvider === "codex") {
      return input.upstreamProvider
    }
    const agentType = normalizeAgentType(input.agentType)
    if (agentType === "claude") return "claude"
    if (agentType === "codex") return "codex"
    return undefined
  }

  private requireNonEmpty(value: string, name: string): string {
    if (value.trim().length === 0) {
      throw new Error(`missing ${name}`)
    }
    return value
  }

  private statusRow(sql: SqlStorage): StatusRow {
    return sql.exec<StatusRow>("SELECT * FROM subrouter_status WHERE id = 1").one()
  }

  private listEligibleAccounts(
    sql: SqlStorage,
    orgId: string,
    quotaKey: string,
    upstreamProvider: UpstreamProvider | undefined
  ): StoredAccount[] {
    return sql
      .exec<AccountRow>(
        `SELECT * FROM accounts
         WHERE org_id = ? AND enabled = 1
         ORDER BY id`,
        orgId
      )
      .toArray()
      .map((row) => this.accountFromRow(row))
      .filter((account) =>
        accountMatchesUpstreamProvider(account, upstreamProvider) &&
        accountHasQuotaForModel(account, quotaKey)
      )
  }

  private getEligibleAccount(
    sql: SqlStorage,
    orgId: string,
    accountId: string,
    quotaKey: string,
    enabledOnly: boolean,
    upstreamProvider: UpstreamProvider | undefined
  ): StoredAccount | null {
    const account = this.getAccount(sql, orgId, accountId, enabledOnly)
    if (!account) return null
    return accountMatchesUpstreamProvider(account, upstreamProvider) &&
      accountHasQuotaForModel(account, quotaKey)
      ? account
      : null
  }

  private getAccount(
    sql: SqlStorage,
    orgId: string,
    accountId: string,
    enabledOnly: boolean
  ): StoredAccount | null {
    const row = this.getAccountRow(sql, orgId, accountId, enabledOnly)
    return row ? this.accountFromRow(row) : null
  }

  private getAccountRow(
    sql: SqlStorage,
    orgId: string,
    accountId: string,
    enabledOnly: boolean
  ): AccountRow | null {
    const where = enabledOnly
      ? "id = ? AND org_id = ? AND enabled = 1"
      : "id = ? AND org_id = ?"
    return (
      sql
        .exec<AccountRow>(`SELECT * FROM accounts WHERE ${where}`, accountId, orgId)
        .toArray()[0] ?? null
    )
  }

  private async refreshAccountIfExpired(
    orgId: string,
    accountId: string,
    force: boolean
  ): Promise<AccountRow | null> {
    const row = this.getAccountRow(this.ctx.storage.sql, orgId, accountId, true)
    if (!row) return null
    const key = `${orgId}:${accountId}`
    const existing = this.refreshInFlight.get(key)
    if (existing) return existing
    const promise = this.refreshAccountRow(row, force).finally(() => {
      this.refreshInFlight.delete(key)
    })
    this.refreshInFlight.set(key, promise)
    return promise
  }

  private async refreshAccountRow(
    row: AccountRow,
    force: boolean
  ): Promise<AccountRow> {
    const kind = row.kind as AccountKind
    if (!isOAuthKind(kind) || !row.credentials_json) return row
    const credentials = JSON.parse(row.credentials_json) as AccountCredentials
    if (!credentials.accessToken || !credentials.refreshToken) return row
    if (!force && !credentialNeedsRefresh(credentials)) return row
    try {
      const refreshed = await refreshOAuthCredentials(kind, credentials, force)
      if (!refreshed.refreshed) return row
      this.updateAccountCredentials(row.id, refreshed.credentials)
      return {
        ...row,
        credentials_json: JSON.stringify(refreshed.credentials),
      }
    } catch (error) {
      const failure = refreshFailureFromError(error)
      const nextCredentials = {
        ...credentials,
        refreshFailure: failure,
      }
      if (terminalRefreshFailure(failure)) {
        this.updateAccountCredentials(row.id, nextCredentials)
      }
      throw error
    }
  }

  private async refreshStatusForRow(
    row: AccountRow,
    forceRefresh: boolean
  ): Promise<OAuthRefreshStatus> {
    const base = safeGoAccount(this.accountFromRow(row))
    if (!isOAuthKind(row.kind as AccountKind)) {
      return {
        ...base,
        auth_checked: false,
        auth_valid: row.credentials_json !== null,
      }
    }
    if (!row.credentials_json) {
      return {
        ...base,
        auth_checked: true,
        auth_valid: false,
        error: "account has no credentials",
      }
    }
    const before = JSON.parse(row.credentials_json) as AccountCredentials
    try {
      const refreshedRow = await this.refreshAccountIfExpired(
        row.org_id,
        row.id,
        forceRefresh
      )
      const after = refreshedRow?.credentials_json
        ? (JSON.parse(refreshedRow.credentials_json) as AccountCredentials)
        : before
      return {
        ...base,
        auth_checked: true,
        auth_valid: Boolean(after.accessToken) && !after.refreshFailure,
        refreshed: before.accessToken !== after.accessToken,
        ...(after.refreshFailure ? { error: after.refreshFailure.message } : {}),
      }
    } catch (error) {
      return {
        ...base,
        auth_checked: true,
        auth_valid: false,
        error: String((error as Error).message ?? error),
      }
    }
  }

  private updateAccountCredentials(
    accountId: string,
    credentials: AccountCredentials
  ): void {
    this.ctx.storage.sql.exec(
      `UPDATE accounts
       SET credentials_json = ?, updated_at = ?
       WHERE id = ?`,
      JSON.stringify(credentials),
      Date.now(),
      accountId
    )
  }

  private async scheduleNextRefreshAlarm(): Promise<void> {
    const rows = this.ctx.storage.sql
      .exec<AccountRow>(
        `SELECT * FROM accounts
         WHERE kind IN ('codex_oauth', 'anthropic_oauth')
           AND credentials_json IS NOT NULL`
      )
      .toArray()
    let next: number | null = null
    const now = Date.now()
    for (const row of rows) {
      const credentials = JSON.parse(row.credentials_json ?? "{}") as AccountCredentials
      if (credentials.refreshFailure && terminalRefreshFailure(credentials.refreshFailure)) {
        continue
      }
      const candidate = nextRefreshAt(credentials, now)
      if (candidate !== null && (next === null || candidate < next)) {
        next = candidate
      }
    }
    if (next === null) {
      await this.ctx.storage.deleteAlarm()
      return
    }
    const current = await this.ctx.storage.getAlarm()
    if (current === null || Math.abs(current - next) > 1_000) {
      await this.ctx.storage.setAlarm(next)
    }
  }

  private accountFromRow(row: AccountRow): StoredAccount {
    const quotas = this.modelQuotasForAccount(row.id)
    return {
      ...rowToAccount(row),
      ...(Object.keys(quotas).length > 0 ? { modelQuotas: quotas } : {}),
    }
  }

  private modelQuotasForAccount(accountId: string): AccountModelQuotas {
    const rows = this.ctx.storage.sql
      .exec<ModelQuotaRow>(
        `SELECT quota_key, remaining_percent, resets_at, protected_below_percent
         FROM account_model_quotas
         WHERE account_id = ?
         ORDER BY quota_key`,
        accountId
      )
      .toArray()
    return quotaRowsToRecord(rows)
  }

  private accountWithLastUsed(
    sql: SqlStorage,
    account: StoredAccount
  ): StoredAccount {
    const usage = sql
      .exec<UsageRow>(
        "SELECT last_used_at FROM account_usage WHERE account_id = ?",
        account.id
      )
      .toArray()[0]
    if (!usage) return account
    return { ...account, lastUsedAt: usage.last_used_at }
  }

  private rememberSticky(
    sql: SqlStorage,
    orgId: string,
    agentType: string,
    quotaKey: string,
    sessionId: string,
    accountId: string,
    userEmail: string | undefined
  ): void {
    const now = Date.now()
    sql.exec(
      `INSERT INTO session_assignments(
        org_id,
        agent_type,
        quota_key,
        session_id,
        account_id,
        user_email,
        created_at,
        updated_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(org_id, agent_type, quota_key, session_id) DO UPDATE SET
        account_id = excluded.account_id,
        user_email = COALESCE(excluded.user_email, session_assignments.user_email),
        updated_at = excluded.updated_at`,
      orgId,
      agentType,
      quotaKey,
      sessionId,
      accountId,
      userEmail ?? null,
      now,
      now
    )
  }

  private isDraining(): boolean {
    const row = this.ctx.storage.sql
      .exec<{ readonly [key: string]: SqlStorageValue; readonly draining: number }>(
        "SELECT draining FROM lifecycle WHERE id = 1"
      )
      .one()
    return row.draining === 1
  }

  private upsertModelQuotas(
    accountId: string,
    quotas: AccountModelQuotas | undefined
  ): void {
    if (!quotas) return
    const sql = this.ctx.storage.sql
    sql.exec("DELETE FROM account_model_quotas WHERE account_id = ?", accountId)
    for (const [quotaKey, quota] of Object.entries(quotas)) {
      sql.exec(
        `INSERT INTO account_model_quotas(
          account_id,
          quota_key,
          remaining_percent,
          resets_at,
          protected_below_percent
        ) VALUES (?, ?, ?, ?, ?)`,
        accountId,
        quotaKey.trim().toLowerCase(),
        quota.remainingPercent,
        quota.resetsAt ?? null,
        quota.protectedBelowPercent ?? null
      )
    }
  }

  private recordUse(sql: SqlStorage, accountId: string): void {
    sql.exec(
      `INSERT OR REPLACE INTO account_usage(account_id, last_used_at)
       VALUES (?, ?)`,
      accountId,
      Date.now()
    )
  }

  private recordRouteMetadata(
    sql: SqlStorage,
    routedAt: number,
    accountId: string,
    sessionId: string
  ): void {
    sql.exec(
      `UPDATE subrouter_status
       SET route_count = route_count + 1,
           last_routed_at = ?,
           last_account_id = ?,
           last_session_id = ?
       WHERE id = 1`,
      routedAt,
      accountId,
      sessionId
    )
  }

  private recordTranscriptEvent(input: TranscriptEventInput): void {
    const orgId = this.resolveOrgId(input.orgId)
    const agentType = normalizeAgentType(input.agentType) || "codex"
    const sessionId = baseSessionId(this.requireNonEmpty(input.sessionId, "sessionId"))
    const event = {
      timestamp: new Date().toISOString(),
      type: input.type,
      payload: input.payload,
    }
    const payloadJSON = JSON.stringify(event.payload)
    this.ctx.storage.sql.exec(
      `INSERT INTO transcript_events(
        org_id,
        agent_type,
        session_id,
        timestamp,
        type,
        payload_json,
        size_bytes
      ) VALUES (?, ?, ?, ?, ?, ?, ?)`,
      orgId,
      agentType,
      sessionId,
      event.timestamp,
      event.type,
      payloadJSON,
      new TextEncoder().encode(JSON.stringify(event)).byteLength
    )
  }

  private listTranscriptRows(orgId?: string): TranscriptEventRow[] {
    return this.ctx.storage.sql
      .exec<TranscriptEventRow>(
        `SELECT * FROM transcript_events
         WHERE org_id = ?
         ORDER BY agent_type, session_id, event_id`,
        this.resolveOrgId(orgId)
      )
      .toArray()
  }

  private recordWebSocketMessage(webSocket: WebSocket): WebSocketAttachment {
    const attachment = webSocketAttachment(webSocket)
    const next = {
      ...attachment,
      lastMessageAt: Date.now(),
      messageCount: attachment.messageCount + 1,
    }
    webSocket.serializeAttachment(next)
    return next
  }

  private async handleWebSocketMessage(
    attachment: WebSocketAttachment,
    message: WebSocketClientMessage
  ): Promise<WebSocketServerMessage> {
    const type = nonEmptyString(message.type)
    const requestId = requestIdFromMessage(message)
    if (!type) {
      throw new Error("WebSocket message is missing type")
    }

    if (type === "ping") {
      return {
        type: "pong",
        ok: true,
        ...(requestId ? { requestId } : {}),
        connectionId: attachment.id,
        connectionCount: this.ctx.getWebSockets().length,
      }
    }

    if (type === "route") {
      const sessionId = nonEmptyString(message.sessionId)
      if (!sessionId) throw new Error("route message is missing sessionId")
      const agentType = normalizeAgentType(nonEmptyString(message.agentType)) || "codex"
      return {
        type: "route",
        ok: true,
        ...(requestId ? { requestId } : {}),
        message: await this.route({
          orgId: websocketOrgId(attachment, message),
          agentType,
          sessionId,
          ...(nonEmptyString(message.preferAccountId)
            ? { preferAccountId: nonEmptyString(message.preferAccountId)! }
            : {}),
          ...(nonEmptyString(message.model) ? { model: nonEmptyString(message.model)! } : {}),
          ...(nonEmptyString(message.quotaKey)
            ? { quotaKey: nonEmptyString(message.quotaKey)! }
            : {}),
        }),
      }
    }

    if (type === "usage") {
      const sessionId = nonEmptyString(message.sessionId)
      if (!sessionId) throw new Error("usage message is missing sessionId")
      const accountId = nonEmptyString(message.accountId)
      if (!accountId) throw new Error("usage message is missing accountId")
      return {
        type: "usage",
        ok: true,
        ...(requestId ? { requestId } : {}),
        message: await this.recordUsage({
          orgId: websocketOrgId(attachment, message),
          sessionId,
          accountId,
        }),
      }
    }

    if (type === "status") {
      websocketOrgId(attachment, message)
      return {
        type: "status",
        ok: true,
        ...(requestId ? { requestId } : {}),
        message: await this.status(),
      }
    }

    throw new Error(`Unknown WebSocket message type ${type}`)
  }
}

const parseJsonOrText = (value: string): unknown => {
  try {
    return JSON.parse(value)
  } catch {
    return value
  }
}

const webSocketAttachment = (webSocket: WebSocket): WebSocketAttachment => {
  const attachment = webSocket.deserializeAttachment() as
    | WebSocketAttachment
    | undefined
  if (attachment) return attachment
  const now = Date.now()
  return {
    id: crypto.randomUUID(),
    orgId: "demo-org",
    connectedAt: now,
    lastMessageAt: null,
    messageCount: 0,
  }
}

const normalizeWebSocketMessage = (
  message: unknown
): ReadonlyArray<WebSocketClientMessage> => {
  if (Array.isArray(message)) {
    return message.map((entry) => normalizeWebSocketMessageEntry(entry))
  }
  if (
    message &&
    typeof message === "object" &&
    !Array.isArray(message) &&
    Array.isArray((message as Record<string, unknown>)["messages"])
  ) {
    return ((message as Record<string, unknown>)["messages"] as unknown[]).map(
      (entry) => normalizeWebSocketMessageEntry(entry)
    )
  }
  return [normalizeWebSocketMessageEntry(message)]
}

const normalizeWebSocketMessageEntry = (
  message: unknown
): WebSocketClientMessage => {
  if (!message || typeof message !== "object" || Array.isArray(message)) {
    throw new Error("WebSocket message must be a JSON object")
  }
  return message as WebSocketClientMessage
}

const requestIdFromMessage = (message: WebSocketClientMessage): string | undefined => {
  const requestId = nonEmptyString(message.requestId)
  return requestId ?? undefined
}

const websocketOrgId = (
  attachment: WebSocketAttachment,
  message: WebSocketClientMessage
): string => {
  const orgId = nonEmptyString(message.orgId) ?? attachment.orgId
  if (orgId !== attachment.orgId) {
    throw new Error("WebSocket message orgId must match the connection orgId")
  }
  return orgId
}

const adminActor = (env: Env, tenantId: string) =>
  env.SUBROUTER_DO.getByName(adminTenantDOName(tenantId))

const tenantScopedAdminActor = (
  env: Env,
  url: URL
): Response | { readonly tenantId: string; readonly actor: DurableObjectStub<SubrouterDurableObject> } => {
  const tenantId = nonEmptyString(url.searchParams.get("tenant"))
  if (!tenantId) {
    return json({ error: "Missing tenant" }, { status: 400 })
  }
  return { tenantId, actor: adminActor(env, tenantId) }
}

const routeInputForTenant = <T extends { readonly orgId?: string }>(
  input: T,
  tenantId: string
): T & { readonly orgId: string } => ({
  ...input,
  orgId: tenantId,
})

const proxyRouteInputForTenant = (
  request: Request,
  input: RouteInput,
  tenantId: string
): RouteInput => {
  const upstreamProvider = upstreamProviderForProxyRequest(request, input)
  return {
    ...input,
    orgId: tenantId,
    upstreamProvider,
    ...(upstreamProvider === "claude" ? { agentType: "claude" } : {}),
  }
}

const upstreamProviderForProxyRequest = (
  request: Request,
  input: RouteInput
): UpstreamProvider => {
  const agentType = normalizeAgentType(input.agentType)
  if (agentType === "claude") return "claude"
  const path = new URL(request.url).pathname
  if (path === "/v1/messages" || path.startsWith("/v1/messages/")) {
    return "claude"
  }
  return "codex"
}

const requestWithTenantOrg = (request: Request, tenantId: string): Request => {
  const url = new URL(request.url)
  url.searchParams.set("orgId", tenantId)
  return new Request(url, request)
}

const validateTenantAccountUpload = async (
  input: UpsertAccountInput,
  options: {
    readonly timeoutMs?: number
    readonly fetcher?: typeof fetch
  } = {}
): Promise<"ok" | "failed"> => {
  if (!input.credentials) return "ok"
  if (!isOAuthKind(input.kind)) return "ok"
  const timeoutMs = options.timeoutMs ?? 10_000
  const signal = AbortSignal.timeout(timeoutMs)
  const baseFetcher = options.fetcher ?? fetch
  const fetcher = Object.assign(
    ((requestInfo: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) =>
      baseFetcher(requestInfo, {
        ...init,
        signal: init?.signal ?? signal,
      })) as typeof fetch,
    { preconnect: baseFetcher.preconnect?.bind(baseFetcher) ?? (() => {}) }
  )
  try {
    await fetchProviderUsage(input.kind, input.credentials, fetcher)
    return "ok"
  } catch {
    return "failed"
  }
}

const validationTimeoutMs = (env: Env): number => {
  const parsed = Number(env.VALIDATION_TIMEOUT_MS)
  if (Number.isFinite(parsed) && parsed > 0 && parsed <= 10_000) {
    return parsed
  }
  return 10_000
}

const defaultUpstreamForAccount = (
  env: Env,
  account: StoredAccountContract
): string => {
  if (providerForAccount(account.kind) === "claude") {
    return env.CLAUDE_UPSTREAM ?? "https://api.anthropic.com"
  }
  if (authModeForAccount(account.kind) === "apikey") {
    return env.API_UPSTREAM ?? "https://api.openai.com/v1"
  }
  return env.CODEX_UPSTREAM ?? "https://chatgpt.com/backend-api/codex"
}

const proxyUpstream = async (
  request: Request,
  env: Env,
  actor: DurableObjectStub<SubrouterDurableObject>,
  routeInput: RouteInput
): Promise<Response> => {
  if (!routeInput.orgId) {
    return json({ error: "tenant id required" }, { status: 401 })
  }
  const routed = await actor.routeForProxy(routeInput)
  const account = routed.account
  if (!account.credentials?.apiKey && !account.credentials?.accessToken) {
    return json(
      { error: "selected account has no usable credential", accountId: account.id },
      { status: 503 }
    )
  }
  const upstreamURL = upstreamURLForRequest(
    request.url,
    account,
    defaultUpstreamForAccount(env, account)
  )
  const headers = setUpstreamAuthHeaders(request.headers, account)
  await actor.recordTranscriptMeta({
    orgId: routeInput.orgId,
    agentType: routeInput.agentType ?? "codex",
    sessionId: routeInput.sessionId,
    type: "subrouter_meta",
    payload: {
      transport: "http",
      ...(routeInput.userEmail ? { user: routeInput.userEmail } : {}),
      account: account.id,
      method: request.method,
      path: new URL(request.url).pathname,
      upstream: upstreamURL.toString(),
      headers: redactedHeaders(stripSubrouterHeaders(request.headers)),
    },
  })
  const init: RequestInit = {
    method: request.method,
    headers,
    redirect: "manual",
  }
  if (request.method !== "GET" && request.method !== "HEAD") {
    await recordTranscriptBodyFromArrayBuffer(actor, {
      orgId: routeInput.orgId,
      agentType: routeInput.agentType ?? "codex",
      sessionId: routeInput.sessionId,
      eventType: "http_body",
      direction: "client_to_upstream",
      body: await request.clone().arrayBuffer(),
    })
    init.body = request.body
  }
  const upstreamRequest = new Request(upstreamURL, init)
  const response = await fetch(upstreamRequest)
  await recordTranscriptBodyFromArrayBuffer(actor, {
    orgId: routeInput.orgId,
    agentType: routeInput.agentType ?? "codex",
    sessionId: routeInput.sessionId,
    eventType: "http_body",
    direction: "upstream_to_client",
    body: await response.clone().arrayBuffer(),
    payload: { status: response.status },
  })
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers: response.headers,
  })
}

const proxyUpstreamWebSocket = async (
  request: Request,
  env: Env,
  actor: DurableObjectStub<SubrouterDurableObject>,
  routeInput: RouteInput
): Promise<Response> => {
  if (!routeInput.orgId) {
    return json({ error: "tenant id required" }, { status: 401 })
  }
  const routed = await actor.routeForProxy(routeInput)
  const account = routed.account
  if (!account.credentials?.apiKey && !account.credentials?.accessToken) {
    return json(
      { error: "selected account has no usable credential", accountId: account.id },
      { status: 503 }
    )
  }
  const upstreamURL = upstreamURLForRequest(
    request.url,
    account,
    defaultUpstreamForAccount(env, account)
  )
  const headers = setUpstreamAuthHeaders(request.headers, account)
  headers.set("Upgrade", "websocket")
  await actor.recordTranscriptMeta({
    orgId: routeInput.orgId,
    agentType: routeInput.agentType ?? "codex",
    sessionId: routeInput.sessionId,
    type: "subrouter_meta",
    payload: {
      transport: "websocket",
      ...(routeInput.userEmail ? { user: routeInput.userEmail } : {}),
      account: account.id,
      method: request.method,
      path: new URL(request.url).pathname,
      upstream: defaultUpstreamForAccount(env, account),
      upstream_url: upstreamURL.toString(),
      headers: redactedHeaders(headers),
    },
  })
  const response = await fetch(
    new Request(upstreamURL, {
      method: "GET",
      headers,
      redirect: "manual",
    })
  )
  if (response.status !== 101 || !response.webSocket) {
    return new Response(await response.text().catch(() => "websocket upgrade failed"), {
      status: response.status === 101 ? 502 : response.status,
      statusText: response.statusText,
      headers: websocketErrorHeaders(response.headers),
    })
  }
  const upstreamSocket = response.webSocket
  upstreamSocket.accept()
  const pair = new WebSocketPair()
  const [client, server] = Object.values(pair) as [WebSocket, WebSocket]
  server.accept()
  bridgeWebSockets({
    actor,
    clientSocket: server,
    upstreamSocket,
    orgId: routeInput.orgId,
    agentType: routeInput.agentType ?? "codex",
    sessionId: routeInput.sessionId,
  })
  return new Response(null, {
    status: 101,
    webSocket: client,
    headers: websocketResponseHeaders(response.headers),
  })
}

const isWebSocketUpgrade = (request: Request): boolean =>
  request.headers.get("Upgrade")?.toLowerCase() === "websocket"

const websocketResponseHeaders = (headers: Headers): Headers => {
  const out = new Headers()
  headers.forEach((value, key) => {
    const lower = key.toLowerCase()
    if (
      lower === "connection" ||
      lower === "upgrade" ||
      lower.startsWith("sec-websocket-")
    ) {
      return
    }
    out.set(key, value)
  })
  return out
}

const websocketErrorHeaders = (headers: Headers): Headers => {
  const out = new Headers()
  const contentType = headers.get("Content-Type")
  if (contentType) out.set("Content-Type", contentType)
  return out
}

const sensitiveHeaders = new Set([
  "authorization",
  "cookie",
  "set-cookie",
  "proxy-authorization",
  "x-api-key",
  "openai-api-key",
  "x-subrouter-proxy-token",
  "x-subrouter-tenant-key",
])

const redactedHeaders = (headers: Headers): Record<string, ReadonlyArray<string>> => {
  const out: Record<string, ReadonlyArray<string>> = {}
  headers.forEach((value, key) => {
    out[key] = sensitiveHeaders.has(key.toLowerCase()) ? ["<redacted>"] : [value]
  })
  return out
}

const recordTranscriptBodyFromArrayBuffer = async (
  actor: DurableObjectStub<SubrouterDurableObject>,
  input: {
    readonly orgId?: string | undefined
    readonly agentType: string
    readonly sessionId: string
    readonly eventType: "http_body" | "websocket_message"
    readonly direction: "client_to_upstream" | "upstream_to_client"
    readonly body: ArrayBuffer
    readonly payload?: Record<string, unknown>
  }
): Promise<void> => {
  await actor.recordTranscriptBody({
    orgId: input.orgId,
    agentType: input.agentType,
    sessionId: input.sessionId,
    eventType: input.eventType,
    direction: input.direction,
    bodyBase64: arrayBufferToBase64(input.body),
    bytes: input.body.byteLength,
    sha256: await sha256Hex(input.body),
    ...(input.payload ? { payload: input.payload } : {}),
  })
}

const bridgeWebSockets = (input: {
  readonly actor: DurableObjectStub<SubrouterDurableObject>
  readonly clientSocket: WebSocket
  readonly upstreamSocket: WebSocket
  readonly orgId?: string | undefined
  readonly agentType: string
  readonly sessionId: string
}): void => {
  input.clientSocket.addEventListener("message", (event) => {
    void relayWebSocketMessage({
      ...input,
      source: input.clientSocket,
      target: input.upstreamSocket,
      direction: "client_to_upstream",
      data: event.data,
    })
  })
  input.upstreamSocket.addEventListener("message", (event) => {
    void relayWebSocketMessage({
      ...input,
      source: input.upstreamSocket,
      target: input.clientSocket,
      direction: "upstream_to_client",
      data: event.data,
    })
  })
  input.clientSocket.addEventListener("close", () => closeWebSocket(input.upstreamSocket))
  input.upstreamSocket.addEventListener("close", () => closeWebSocket(input.clientSocket))
  input.clientSocket.addEventListener("error", () => closeWebSocket(input.upstreamSocket))
  input.upstreamSocket.addEventListener("error", () => closeWebSocket(input.clientSocket))
}

const relayWebSocketMessage = async (input: {
  readonly actor: DurableObjectStub<SubrouterDurableObject>
  readonly source: WebSocket
  readonly target: WebSocket
  readonly orgId?: string | undefined
  readonly agentType: string
  readonly sessionId: string
  readonly direction: "client_to_upstream" | "upstream_to_client"
  readonly data: unknown
}): Promise<void> => {
  if (input.source.readyState !== WebSocket.OPEN || input.target.readyState !== WebSocket.OPEN) {
    return
  }
  const message = await webSocketMessageToSendable(input.data)
  if (!message) return
  await recordTranscriptBodyFromArrayBuffer(input.actor, {
    orgId: input.orgId,
    agentType: input.agentType,
    sessionId: input.sessionId,
    eventType: "websocket_message",
    direction: input.direction,
    body: message.body,
    payload: { opcode: message.opcode },
  })
  input.target.send(message.send)
}

const webSocketMessageToSendable = async (
  data: unknown
): Promise<{ readonly send: string | ArrayBuffer; readonly body: ArrayBuffer; readonly opcode: string } | null> => {
  if (typeof data === "string") {
    return {
      send: data,
      body: new TextEncoder().encode(data).buffer,
      opcode: "text",
    }
  }
  if (data instanceof ArrayBuffer) {
    return { send: data, body: data, opcode: "binary" }
  }
  if (ArrayBuffer.isView(data)) {
    const body = new Uint8Array(
      new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
    ).buffer as ArrayBuffer
    return { send: body, body, opcode: "binary" }
  }
  if (data instanceof Blob) {
    const body = await data.arrayBuffer()
    return { send: body, body, opcode: "binary" }
  }
  return null
}

const closeWebSocket = (webSocket: WebSocket): void => {
  if (webSocket.readyState === WebSocket.OPEN || webSocket.readyState === WebSocket.CONNECTING) {
    try {
      webSocket.close()
    } catch {
      // The peer may already be closing.
    }
  }
}

const arrayBufferToBase64 = (body: ArrayBuffer): string => {
  const bytes = new Uint8Array(body)
  let binary = ""
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000))
  }
  return btoa(binary)
}

const sha256Hex = async (body: ArrayBuffer): Promise<string> => {
  const digest = await crypto.subtle.digest("SHA-256", body)
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("")
}

const parseTranscriptPath = (
  path: string
): { readonly agentType: string; readonly sessionId: string; readonly raw: boolean } | null => {
  const prefix = "/_subrouter/transcripts/"
  if (!path.startsWith(prefix)) return null
  let rest = path.slice(prefix.length)
  let raw = false
  if (rest.endsWith("/raw")) {
    raw = true
    rest = rest.slice(0, -"/raw".length)
  }
  const slash = rest.indexOf("/")
  if (slash <= 0 || slash === rest.length - 1) return null
  try {
    const agentType = decodeURIComponent(rest.slice(0, slash))
    const sessionId = decodeURIComponent(rest.slice(slash + 1))
    if (!agentType || !sessionId) return null
    return { agentType, sessionId, raw }
  } catch {
    return null
  }
}

const renderDashboard = (
  data: TranscriptDashboardData,
  orgId: string
): string => {
  const summaries = data.summaries
  const analytics = data.analytics
  const totalEvents = summaries.reduce(
    (sum, item) => sum + Number(item["event_count"] ?? 0),
    0
  )
  const modelRows = usageGroupRows(analytics.by_model, "Model", analytics.by_model.length ? analytics.by_model[0]!.usage.total_tokens : 0)
  const userRows = usageGroupRows(analytics.by_user, "User", analytics.max_user_tokens)
  const accountRows = usageGroupRows(analytics.by_account, "Account", analytics.max_account_tokens)
  const timelineBars = analytics.timeline
    .map((bucket) => {
      const height = usageBarPercent(bucket.usage.total_tokens, analytics.max_timeline_tokens)
      return `<div class="bar" style="height:${height}%" title="${escapeHTML(bucket.label)}: ${bucket.usage.total_tokens} tokens"></div>`
    })
    .join("")
  const rows = summaries
    .map((summary) => {
      const agentType = String(summary["agent_type"] ?? "")
      const sessionId = String(summary["session_id"] ?? "")
      const usage = recordPayload(summary["usage"]) ?? {}
      const href = `/_subrouter/transcripts/${encodeURIComponent(agentType)}/${encodeURIComponent(sessionId)}?tenant=${encodeURIComponent(orgId)}`
      const raw = `${href.replace("?", "/raw?")}`
      return `<tr>
        <td>${escapeHTML(agentType)}</td>
        <td><a href="${href}">${escapeHTML(sessionId)}</a></td>
        <td>${escapeHTML(String(summary["user"] ?? ""))}</td>
        <td>${escapeHTML(String(summary["account"] ?? ""))}</td>
        <td>${Number(summary["event_count"] ?? 0)}</td>
        <td>${Number(summary["total_bytes"] ?? 0)}</td>
        <td>${Number(usage["total_tokens"] ?? 0)}</td>
        <td>${escapeHTML(String(summary["last_event_at"] ?? ""))}</td>
        <td><a href="${raw}">raw</a></td>
      </tr>`
    })
    .join("")
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Subrouter Trajectories</title>
  <style>
    :root { color-scheme: light dark; --border: #74747455; --muted: #777; --fill: #4f7cff; --fill-soft: #4f7cff33; }
    body { font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 24px; line-height: 1.35; }
    header { display: flex; justify-content: space-between; gap: 24px; align-items: baseline; margin-bottom: 20px; }
    h1 { margin: 0; font-size: 24px; }
    h2 { margin-top: 28px; font-size: 18px; }
    h3 { margin: 0 0 10px; font-size: 14px; color: var(--muted); }
    .metrics { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; margin-bottom: 20px; }
    .metric { border: 1px solid var(--border); border-radius: 8px; padding: 10px 12px; min-width: 120px; }
    .value { font-size: 22px; font-weight: 700; }
    .label { color: var(--muted); font-size: 12px; }
    .charts { display: grid; grid-template-columns: minmax(0, 1.5fr) minmax(320px, 1fr); gap: 16px; align-items: start; }
    .split { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; margin-top: 16px; }
    .panel { border: 1px solid var(--border); border-radius: 8px; padding: 12px; }
    .timeline { display: flex; align-items: end; gap: 2px; height: 180px; border-bottom: 1px solid var(--border); padding-top: 12px; }
    .timeline .bar { flex: 1; min-width: 3px; background: var(--fill); border-radius: 3px 3px 0 0; }
    .hbar { display: grid; grid-template-columns: minmax(120px, 1fr) minmax(140px, 2fr) 80px; gap: 10px; align-items: center; margin: 8px 0; font-size: 13px; }
    .track { height: 10px; background: var(--fill-soft); border-radius: 999px; overflow: hidden; }
    .track span { display: block; height: 100%; background: var(--fill); }
    table { border-collapse: collapse; width: 100%; font-size: 13px; }
    th, td { border-bottom: 1px solid var(--border); padding: 8px; text-align: left; vertical-align: top; }
    th { color: var(--muted); font-weight: 600; }
    a { color: inherit; }
    @media (max-width: 900px) { .metrics, .charts, .split { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
  <header>
    <h1>Subrouter Trajectories</h1>
    <div class="label">org ${escapeHTML(orgId)}</div>
  </header>
  <section class="metrics">
    <div class="metric"><div class="value">${summaries.length}</div><div class="label">Sessions</div></div>
    <div class="metric"><div class="value">${totalEvents}</div><div class="label">Events</div></div>
    <div class="metric"><div class="value">${formatTokens(analytics.totals.total_tokens)}</div><div class="label">Total Tokens</div></div>
    <div class="metric"><div class="value">${analytics.totals.requests}</div><div class="label">Model Calls</div></div>
  </section>
  <h2>Usage</h2>
  <section class="charts">
    <div class="panel">
      <h3>Tokens Over Time</h3>
      <div class="timeline">${timelineBars || `<span class="label">No token usage found in transcripts.</span>`}</div>
    </div>
    <div class="panel">
      <h3>Models</h3>
      ${modelRows || `<p class="label">No model usage found.</p>`}
    </div>
  </section>
  <section class="split">
    <div class="panel">
      <h3>Users</h3>
      ${userRows || `<p class="label">No user usage found.</p>`}
    </div>
    <div class="panel">
      <h3>Accounts</h3>
      ${accountRows || `<p class="label">No account usage found.</p>`}
    </div>
  </section>
  <h2>Transcripts</h2>
  <table>
    <thead><tr><th>Agent</th><th>Session</th><th>User</th><th>Account</th><th>Events</th><th>Bytes</th><th>Total Tokens</th><th>Last Event</th><th></th></tr></thead>
    <tbody>${rows || `<tr><td colspan="9" class="label">No transcripts found.</td></tr>`}</tbody>
  </table>
</body>
</html>`
}

const usageGroupRows = (
  groups: ReadonlyArray<TranscriptUsageGroup>,
  label: string,
  maxTokens: number
): string =>
  groups
    .map((group) => {
      const width = usageBarPercent(group.usage.total_tokens, maxTokens)
      return `<div class="hbar">
        <div title="${escapeHTML(label)}">${escapeHTML(group.key)}</div>
        <div class="track"><span style="width:${width}%"></span></div>
        <div>${formatTokens(group.usage.total_tokens)}</div>
      </div>`
    })
    .join("")

const usageBarPercent = (value: number, max: number): number => {
  if (max <= 0 || value <= 0) return 0
  return Math.max(1, Math.round((value * 100) / max))
}

const formatTokens = (value: number): string => {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}k`
  return String(value)
}

const escapeHTML = (value: string): string =>
  value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;")

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url)

    if (url.pathname === "/healthz") {
      return json({ ok: true, service: "subrouter-do" })
    }

    if (url.pathname === "/_subrouter/health") {
      return json({ ok: true })
    }

    if (url.pathname === "/_subrouter/ready") {
      return json({ ok: true })
    }

    if (url.pathname === "/ws") {
      if (request.method !== "GET") {
        return json({ error: "WebSocket endpoint requires GET" }, { status: 405 })
      }
      if (request.headers.get("Upgrade")?.toLowerCase() !== "websocket") {
        return json(
          { error: "Expected WebSocket Upgrade request" },
          { status: 426 }
        )
      }
      const tenant = await authorizeTenant(request, env)
      if (tenant instanceof Response) return tenant
      return tenantActor(env, tenant).fetch(requestWithTenantOrg(request, tenant.tenantId))
    }

    if (url.pathname === "/websocket-status") {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      return json(await scoped.actor.webSocketStats())
    }

    if (url.pathname === "/route" && request.method === "POST") {
      const tenant = await authorizeTenant(request, env)
      if (tenant instanceof Response) return tenant
      try {
        const input = await parseRouteInput(request)
        const actor = tenantActor(env, tenant)
        return json(await actor.route(routeInputForTenant(input, tenant.tenantId)))
      } catch (error) {
        return errorJson(error, 409)
      }
    }

    if (url.pathname === "/_subrouter/drain") {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      if (request.method !== "POST") {
        return json({ error: "method not allowed" }, { status: 405 })
      }
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      return json(await scoped.actor.drain())
    }

    if (url.pathname === "/_subrouter/drain-status") {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      if (request.method !== "GET") {
        return json({ error: "method not allowed" }, { status: 405 })
      }
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      return json(await scoped.actor.ready())
    }

    if (url.pathname === "/usage" && request.method === "POST") {
      const tenant = await authorizeTenant(request, env)
      if (tenant instanceof Response) return tenant
      const input = await parseUsageInput(request)
      if (input instanceof Response) {
        return input
      }
      const actor = tenantActor(env, tenant)
      try {
        return json(await actor.recordUsage(routeInputForTenant(input, tenant.tenantId)))
      } catch (error) {
        return errorJson(error, 400)
      }
    }

    if (url.pathname === "/status") {
      const tenant = await authorizeTenant(request, env)
      if (tenant instanceof Response) return tenant
      const actor = tenantActor(env, tenant)
      try {
        return json(await actor.status())
      } catch (error) {
        return errorJson(error, 400)
      }
    }

    if (url.pathname === "/_subrouter/sessions") {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      return json(await scoped.actor.listSessions())
    }

    if (url.pathname === "/_subrouter/accounts") {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      const { accounts } = await scoped.actor.listAccounts(scoped.tenantId)
      return json(accounts.map((account) => safeGoAccount(account)))
    }

    if (url.pathname === "/_subrouter/account-status") {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      if (request.method !== "GET" && request.method !== "POST") {
        return json({ error: "method not allowed" }, { status: 405 })
      }
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      return json(await scoped.actor.accountStatuses(scoped.tenantId, request.method === "POST"))
    }

    if (url.pathname === "/_subrouter/usage-status") {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      if (request.method !== "GET") {
        return json({ error: "method not allowed" }, { status: 405 })
      }
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      return json(await scoped.actor.usageStatuses(scoped.tenantId))
    }

    if (url.pathname === "/_subrouter/reload-accounts") {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      if (request.method !== "POST") {
        return json({ error: "method not allowed" }, { status: 405 })
      }
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      const { accounts } = await scoped.actor.listAccounts(scoped.tenantId)
      return json({ ok: true, accounts: accounts.length, usage_refreshed: 0 })
    }

    if (url.pathname === "/_subrouter/dashboard") {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      return new Response(renderDashboard(await scoped.actor.transcriptDashboardData(scoped.tenantId), scoped.tenantId), {
        headers: { "Content-Type": "text/html; charset=utf-8" },
      })
    }

    if (
      url.pathname === "/_subrouter/transcripts" ||
      url.pathname.startsWith("/_subrouter/transcripts/")
    ) {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized
      const scoped = tenantScopedAdminActor(env, url)
      if (scoped instanceof Response) return scoped
      if (url.pathname === "/_subrouter/transcripts") {
        return json(await scoped.actor.listTranscriptSummaries(scoped.tenantId))
      }
      const parsed = parseTranscriptPath(url.pathname)
      if (!parsed) return new Response("Not Found", { status: 404 })
      try {
        return json(await scoped.actor.readTranscriptSession({ orgId: scoped.tenantId, ...parsed }))
      } catch (error) {
        return errorJson(error, 404)
      }
    }

    if (url.pathname === "/tenant/accounts" && request.method === "POST") {
      const tenant = await authorizeTenant(request, env)
      if (tenant instanceof Response) return tenant
      if (tenant.legacy) {
        return json({ error: "Tenant key required" }, { status: 401 })
      }
      const input = await parseTenantAccountUploadInput(request, tenant.tenantId)
      if (input instanceof Response) return input
      const validation = input.validate
        ? await validateTenantAccountUpload(input.account, {
            timeoutMs: validationTimeoutMs(env),
          })
        : undefined
      if (validation === "failed") {
        return json({ error: "Account validation failed", validation }, { status: 400 })
      }
      const actor = tenantActor(env, tenant)
      try {
        const { account } = await actor.upsertAccount(input.account)
        return json(safeTenantAccount(account, validation))
      } catch (error) {
        return errorJson(error, 400)
      }
    }

    if (url.pathname === "/tenant/accounts" && request.method === "GET") {
      const tenant = await authorizeTenant(request, env)
      if (tenant instanceof Response) return tenant
      if (tenant.legacy) {
        return json({ error: "Tenant key required" }, { status: 401 })
      }
      const { accounts } = await tenantActor(env, tenant).listAccounts(tenant.tenantId)
      return json({ accounts: accounts.map((account) => safeTenantAccount(account)) })
    }

    const tenantAccountDeleteMatch = url.pathname.match(/^\/tenant\/accounts\/([^/]+)$/)
    if (tenantAccountDeleteMatch && request.method === "DELETE") {
      const tenant = await authorizeTenant(request, env)
      if (tenant instanceof Response) return tenant
      if (tenant.legacy) {
        return json({ error: "Tenant key required" }, { status: 401 })
      }
      const accountId = decodeURIComponent(tenantAccountDeleteMatch[1]!)
      try {
        return json(
          await tenantActor(env, tenant).deleteAccount({
            orgId: tenant.tenantId,
            accountId,
          })
        )
      } catch (error) {
        return errorJson(error, 404)
      }
    }

    if (url.pathname.startsWith("/admin/")) {
      const unauthorized = authorizeAdmin(request, env)
      if (unauthorized) return unauthorized

      if (url.pathname === "/admin/tenants" && request.method === "POST") {
        const record = await parseJsonRecord(request)
        if (record instanceof Response) return record
        try {
          return json(await registryActor(env).createTenant({ name: record["name"] }))
        } catch (error) {
          return errorJson(error, 400)
        }
      }

      if (url.pathname === "/admin/tenants" && request.method === "GET") {
        return json(await registryActor(env).listTenants())
      }

      const tenantRevokeMatch = url.pathname.match(/^\/admin\/tenants\/([^/]+)\/revoke$/)
      if (tenantRevokeMatch && request.method === "POST") {
        const tenantId = decodeURIComponent(tenantRevokeMatch[1]!)
        try {
          return json(await registryActor(env).revokeTenant(tenantId))
        } catch (error) {
          return errorJson(error, 404)
        }
      }

      const tenantRotateMatch = url.pathname.match(/^\/admin\/tenants\/([^/]+)\/rotate$/)
      if (tenantRotateMatch && request.method === "POST") {
        const tenantId = decodeURIComponent(tenantRotateMatch[1]!)
        try {
          return json(await registryActor(env).rotateTenant(tenantId))
        } catch (error) {
          return errorJson(error, 400)
        }
      }

      if (url.pathname === "/admin/accounts" && request.method === "GET") {
        const scoped = tenantScopedAdminActor(env, url)
        if (scoped instanceof Response) return scoped
        return json(await scoped.actor.listAccounts(scoped.tenantId))
      }

      if (url.pathname === "/admin/accounts" && request.method === "POST") {
        try {
          const input = await parseUpsertAccountInput(request)
          if (input instanceof Response) return input
          return json(await adminActor(env, input.orgId).upsertAccount(input))
        } catch (error) {
          return json({ error: String((error as Error).message ?? error) }, { status: 400 })
        }
      }

      const totpMatch = url.pathname.match(/^\/admin\/accounts\/([^/]+)\/totp$/)
      if (totpMatch && request.method === "POST") {
        const accountId = decodeURIComponent(totpMatch[1]!)
        const scoped = tenantScopedAdminActor(env, url)
        if (scoped instanceof Response) return scoped
        try {
          return json(await scoped.actor.totpCode({ orgId: scoped.tenantId, accountId }))
        } catch (error) {
          return errorJson(error, 400)
        }
      }

      if (url.pathname === "/admin/model-probe" && request.method === "POST") {
        try {
          const input = await parseModelProbeInput(request)
          if (input instanceof Response) return input
          const tenantId = input.orgId ?? nonEmptyString(url.searchParams.get("tenant"))
          if (!tenantId) return json({ error: "Missing tenant" }, { status: 400 })
          return json(await adminActor(env, tenantId).modelProbe(routeInputForTenant(input, tenantId)))
        } catch (error) {
          return errorJson(error, 400)
        }
      }
    }

    if (url.pathname.startsWith("/tenant/")) {
      return new Response("Not Found", { status: 404 })
    }

    if (url.pathname.startsWith("/_subrouter/")) {
      return new Response("Not Found", { status: 404 })
    }

    const tenant = await authorizeTenant(request, env)
    if (tenant instanceof Response) return tenant
    try {
      const routeInput = await parseProxyRouteInput(request)
      const tenantRouteInput = proxyRouteInputForTenant(request, routeInput, tenant.tenantId)
      const actor = tenantActor(env, tenant)
      if (isWebSocketUpgrade(request)) {
        return await proxyUpstreamWebSocket(request, env, actor, tenantRouteInput)
      }
      return await proxyUpstream(request, env, actor, tenantRouteInput)
    } catch (error) {
      return errorJson(error, 503)
    }
  },
} satisfies ExportedHandler<Env>
