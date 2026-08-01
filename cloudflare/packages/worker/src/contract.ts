import type { Account } from "@subrouter/core"

export type AccountKind = Account["kind"]
export type AgentType = "codex" | "claude" | "gemini" | string

export interface AccountCredentials {
  readonly apiKey?: string
  readonly accessToken?: string
  readonly refreshToken?: string
  readonly idToken?: string
  readonly accountId?: string
  readonly baseUrl?: string
  readonly expiresAt?: number
  readonly tokenEndpoint?: string
  readonly clientId?: string
  readonly lastRefreshAt?: number
  readonly credentialGeneration?: number
  readonly refreshFailure?: OAuthRefreshFailure
  readonly usageUrl?: string
  readonly [key: string]: unknown
}

export interface OAuthRefreshFailure {
  readonly at: number
  readonly status: number
  readonly message: string
  readonly providerCode?: string
  readonly providerType?: string
}

export interface OAuthRefreshResult {
  readonly credentials: AccountCredentials
  readonly refreshed: boolean
}

const CODEX_OAUTH_TOKEN_URL = "https://auth.openai.com/oauth/token"
const CODEX_OAUTH_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
const CLAUDE_OAUTH_TOKEN_URL = "https://platform.claude.com/v1/oauth/token"
const CLAUDE_OAUTH_CLIENT_ID = "https://claude.ai/oauth/claude-code-client-metadata"
const CLAUDE_OAUTH_BETA = "oauth-2025-04-20"
const CODEX_USAGE_URL = "https://chatgpt.com/backend-api/wham/usage"
const CLAUDE_USAGE_URL = "https://api.anthropic.com/api/oauth/usage"
const REFRESH_SKEW_MS = 60_000

export interface UsageWindow {
  readonly name: string
  readonly used_percent: number
  readonly limit_window_seconds?: number
  readonly reset_after_seconds?: number
  readonly feature?: string
}

export interface CreditsInfo {
  readonly has_credits: boolean
  readonly unlimited: boolean
  readonly balance: string
}

export interface ProviderUsageStatus {
  readonly plan_type?: string
  readonly windows?: ReadonlyArray<UsageWindow>
  readonly credits?: CreditsInfo
}

export interface StoredAccountContract extends Account {
  readonly hasCredentials: boolean
  readonly hasTotp: boolean
  readonly credentials?: AccountCredentials
  readonly source?: string
}

export interface ProxyRouteInput {
  readonly orgId: string
  readonly agentType: AgentType
  readonly sessionId: string
  readonly userEmail?: string
  readonly preferAccountId?: string
  readonly model?: string
}

const sessionHeaders = [
  "X-Subrouter-Session",
  "X-Codex-Window-ID",
  "X-Codex-Turn-State",
  "X-Codex-Parent-Thread-ID",
  "X-Session-ID",
  "X-Conversation-ID",
  "X-Codex-Session-ID",
  "X-Claude-Session-ID",
  "X-Claude-Code-Session-Id",
  "X-Gemini-Session-ID",
  "X-Gemini-Conversation-ID",
  "OpenAI-Conversation-ID",
  "Anthropic-Conversation-ID",
  "Google-Conversation-ID",
  "Idempotency-Key",
] as const

const userEmailHeaders = [
  "X-Subrouter-User-Email",
  "X-Subrouter-User",
  "X-User-Email",
] as const

const accountHeaders = ["X-Subrouter-Account-ID", "X-Subrouter-Account"] as const
const modelHeaders = ["X-Subrouter-Model", "X-Model"] as const
const orgHeaders = ["X-Subrouter-Org-ID", "X-Regatta-Org-ID"] as const

const subrouterHeadersToStrip = [
  "X-Subrouter-Session",
  "X-Subrouter-Agent",
  "X-Subrouter-User-Email",
  "X-Subrouter-User",
  "X-User-Email",
  "X-Subrouter-Account-ID",
  "X-Subrouter-Account",
  "X-Subrouter-Model",
  "X-Model",
  "X-Subrouter-Org-ID",
  "X-Regatta-Org-ID",
  "X-Subrouter-Tenant-Key",
  "X-Subrouter-Proxy-Token",
] as const

export const normalizeAgentType = (value: string | null | undefined): AgentType => {
  const normalized = value?.trim().toLowerCase() ?? ""
  if (!normalized || normalized.length > 64) return ""
  return /^[a-z0-9._-]+$/.test(normalized) ? normalized : ""
}

export const normalizeAccountId = (value: string | null | undefined): string => {
  const trimmed = value?.trim() ?? ""
  return trimmed.length > 0 && trimmed.length <= 256 ? trimmed : ""
}

export const normalizeModel = (value: string | null | undefined): string => {
  const trimmed = value?.trim() ?? ""
  return trimmed.length > 0 && trimmed.length <= 256 ? trimmed : ""
}

export const normalizeUserEmail = (value: string | null | undefined): string => {
  const trimmed = value?.trim() ?? ""
  if (!trimmed || trimmed.length > 320) return ""
  const match = trimmed.match(/<?([^<>\s@]+@[^<>\s@]+\.[^<>\s@]+)>?$/)
  return match?.[1]?.toLowerCase() ?? ""
}

export const normalizeOrgId = (value: string | null | undefined): string => {
  const trimmed = value?.trim() ?? ""
  if (!trimmed || trimmed.length > 256) return ""
  return /^[A-Za-z0-9._:-]+$/.test(trimmed) ? trimmed : ""
}

const firstHeader = (
  headers: Headers,
  candidates: ReadonlyArray<string>,
  normalize: (value: string | null | undefined) => string
): string => {
  for (const header of candidates) {
    const value = normalize(headers.get(header))
    if (value) return value
  }
  return ""
}

export const inferAgentType = (headers: Headers): AgentType => {
  const explicit = normalizeAgentType(headers.get("X-Subrouter-Agent"))
  if (explicit) return explicit
  const userAgent = headers.get("User-Agent")?.toLowerCase() ?? ""
  if (userAgent.includes("claude-cli") || userAgent.includes("claude-code")) {
    return "claude"
  }
  if (
    headers.has("X-Claude-Session-ID") ||
    headers.has("X-Claude-Code-Session-Id") ||
    headers.has("Anthropic-Conversation-ID")
  ) {
    return "claude"
  }
  if (
    headers.has("X-Gemini-Session-ID") ||
    headers.has("X-Gemini-Conversation-ID") ||
    headers.has("Google-Conversation-ID")
  ) {
    return "gemini"
  }
  return "codex"
}

export const stickySessionId = (agentType: AgentType, sessionId: string): string => {
  if (sessionId.startsWith("fallback:")) return sessionId
  if (normalizeAgentType(agentType) !== "codex") return sessionId
  return sessionId.split(":", 1)[0] ?? sessionId
}

export const scopedSessionKey = (agentType: AgentType, sessionId: string): string =>
  `${normalizeAgentType(agentType) || "codex"}:${stickySessionId(agentType, sessionId)}`

export const resolveOrgId = (request: Request, fallback = "demo-org"): string => {
  const url = new URL(request.url)
  const fromHeader = firstHeader(request.headers, orgHeaders, normalizeOrgId)
  if (fromHeader) return fromHeader
  return normalizeOrgId(url.searchParams.get("orgId")) || fallback
}

export const parseProxyRouteInput = async (
  request: Request,
  fallbackOrgId = "demo-org"
): Promise<ProxyRouteInput> => {
  const url = new URL(request.url)
  const body = await parseJsonBody(request)
  const agentType = inferAgentType(request.headers) || "codex"
  const orgId = resolveOrgId(request, fallbackOrgId)
  const sessionId =
    firstHeader(request.headers, sessionHeaders, (value) => value?.trim() ?? "") ||
    normalizeString(url.searchParams.get("session_id")) ||
    normalizeString(url.searchParams.get("conversation_id")) ||
    normalizeString(url.searchParams.get("thread_id")) ||
    findJsonString(body, ["session_id", "conversation_id", "thread_id"]) ||
    await fallbackSessionId(request)
  const userEmail = firstHeader(request.headers, userEmailHeaders, normalizeUserEmail)
  const preferAccountId = firstHeader(request.headers, accountHeaders, normalizeAccountId)
  const model =
    firstHeader(request.headers, modelHeaders, normalizeModel) ||
    normalizeModel(url.searchParams.get("model")) ||
    findJsonString(body, ["model"])

  return {
    orgId,
    agentType,
    sessionId: stickySessionId(agentType, sessionId),
    ...(userEmail ? { userEmail } : {}),
    ...(preferAccountId ? { preferAccountId } : {}),
    ...(model ? { model } : {}),
  }
}

const parseJsonBody = async (request: Request): Promise<unknown> => {
  if (!request.headers.get("Content-Type")?.includes("json")) return null
  const text = await request.clone().text().catch(() => "")
  if (!text) return null
  try {
    return JSON.parse(text)
  } catch {
    return null
  }
}

const findJsonString = (
  value: unknown,
  keys: ReadonlyArray<string>
): string => {
  if (!value || typeof value !== "object") return ""
  if (Array.isArray(value)) {
    for (const entry of value) {
      const found = findJsonString(entry, keys)
      if (found) return found
    }
    return ""
  }
  const record = value as Record<string, unknown>
  for (const key of keys) {
    const found = normalizeString(record[key])
    if (found) return found
  }
  for (const entry of Object.values(record)) {
    const found = findJsonString(entry, keys)
    if (found) return found
  }
  return ""
}

const normalizeString = (value: unknown): string =>
  typeof value === "string" && value.trim().length > 0 ? value.trim() : ""

const fallbackSessionId = async (request: Request): Promise<string> => {
  const url = new URL(request.url)
  const remote =
    request.headers.get("CF-Connecting-IP") ??
    request.headers.get("X-Forwarded-For")?.split(",", 1)[0]?.trim() ??
    ""
  const userAgent = request.headers.get("User-Agent") ?? ""
  const input = `${remote}\0${userAgent}\0${request.method}\0${url.pathname}`
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(input))
  const hex = [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("")
  return `fallback:${hex.slice(0, 24)}`
}

export const stripSubrouterHeaders = (headers: Headers): Headers => {
  const out = new Headers(headers)
  for (const header of subrouterHeadersToStrip) out.delete(header)
  return out
}

export const providerForAccount = (kind: AccountKind): "codex" | "claude" => {
  if (kind === "anthropic_oauth" || kind === "anthropic_apikey") return "claude"
  return "codex"
}

export const authModeForAccount = (kind: AccountKind): "oauth" | "apikey" => {
  return kind === "openai_apikey" || kind === "anthropic_apikey" ? "apikey" : "oauth"
}

export const isOAuthKind = (kind: AccountKind): boolean =>
  authModeForAccount(kind) === "oauth"

export const safeAccount = (account: StoredAccountContract) => ({
  id: account.id,
  provider: providerForAccount(account.kind),
  auth_mode: authModeForAccount(account.kind),
  ...(account.label.includes("@") ? { email: account.label } : {}),
  source: account.source ?? "durable-object",
})

export const accountStatus = (account: StoredAccountContract) => ({
  ...safeAccount(account),
  auth_checked: account.hasCredentials,
  auth_valid: account.hasCredentials,
})

export const usageStatus = (account: StoredAccountContract) => ({
  ...accountStatus(account),
  plan_type: authModeForAccount(account.kind) === "apikey" ? "api key" : providerForAccount(account.kind),
  windows: Object.entries(account.modelQuotas ?? {}).map(([name, quota]) => ({
    name,
    used_percent: Math.max(0, 100 - quota.remainingPercent),
    reset_after_seconds: quota.resetsAt
      ? Math.max(0, Math.floor((quota.resetsAt - Date.now()) / 1000))
      : 0,
  })),
})

export const fetchProviderUsage = async (
  kind: AccountKind,
  credentials: AccountCredentials,
  fetcher: typeof fetch = fetch
): Promise<ProviderUsageStatus> => {
  if (!credentials.accessToken) return {}
  if (providerForAccount(kind) === "claude") {
    return fetchClaudeUsage(credentials, fetcher)
  }
  if (authModeForAccount(kind) === "oauth") {
    return fetchCodexUsage(credentials, fetcher)
  }
  return { plan_type: "api key" }
}

const fetchCodexUsage = async (
  credentials: AccountCredentials,
  fetcher: typeof fetch
): Promise<ProviderUsageStatus> => {
  const response = await fetcher(credentials.usageUrl ?? CODEX_USAGE_URL, {
    method: "GET",
    headers: {
      Authorization: `Bearer ${credentials.accessToken}`,
      ...(credentials.accountId ? { "ChatGPT-Account-ID": credentials.accountId } : {}),
    },
  })
  if (!response.ok) {
    throw new Error(`usage fetch failed: ${response.status} ${response.statusText}`.trim())
  }
  const payload = (await response.json()) as CodexUsageResponse
  return {
    ...(payload.plan_type ? { plan_type: payload.plan_type } : {}),
    windows: codexDisplayWindows(payload),
    ...(payload.credits
      ? {
          credits: {
            has_credits: payload.credits.has_credits,
            unlimited: payload.credits.unlimited,
            balance: payload.credits.balance,
          },
        }
      : {}),
  }
}

const fetchClaudeUsage = async (
  credentials: AccountCredentials,
  fetcher: typeof fetch
): Promise<ProviderUsageStatus> => {
  const response = await fetcher(credentials.usageUrl ?? CLAUDE_USAGE_URL, {
    method: "GET",
    headers: {
      Authorization: `Bearer ${credentials.accessToken}`,
      "anthropic-beta": CLAUDE_OAUTH_BETA,
      "Content-Type": "application/json",
    },
  })
  if (!response.ok) {
    throw new Error(`usage fetch failed: ${response.status} ${response.statusText}`.trim())
  }
  const payload = (await response.json()) as ClaudeUsageResponse
  const windows: UsageWindow[] = []
  appendClaudeRateLimit(windows, "5h", payload.five_hour)
  appendClaudeRateLimit(windows, "7d", payload.seven_day)
  appendClaudeRateLimit(windows, "opus-weekly", payload.seven_day_opus)
  appendClaudeRateLimit(windows, "sonnet-weekly", payload.seven_day_sonnet)
  if (payload.extra_usage?.is_enabled && typeof payload.extra_usage.utilization === "number") {
    windows.push({ name: "extra", used_percent: payload.extra_usage.utilization })
  }
  return { plan_type: "claude", windows }
}

interface CodexUsageResponse {
  readonly plan_type?: string
  readonly rate_limit?: CodexRateLimitDetails
  readonly credits?: {
    readonly has_credits: boolean
    readonly unlimited: boolean
    readonly balance: string
  }
  readonly additional_rate_limits?: ReadonlyArray<{
    readonly metered_feature?: string
    readonly limit_name?: string
    readonly rate_limit?: CodexRateLimitDetails
  }>
}

interface CodexRateLimitDetails {
  readonly limit_reached?: boolean
  readonly primary_window?: CodexLimitWindow
  readonly secondary_window?: CodexLimitWindow
}

interface CodexLimitWindow {
  readonly used_percent: number
  readonly limit_window_seconds?: number
  readonly reset_after_seconds?: number
  readonly reset_at?: number
}

const codexDisplayWindows = (usage: CodexUsageResponse): UsageWindow[] => {
  const windows: UsageWindow[] = []
  appendCodexWindows(windows, "", undefined, usage.rate_limit)
  for (const additional of usage.additional_rate_limits ?? []) {
    const name = additional.limit_name || additional.metered_feature || "unknown"
    appendCodexWindows(windows, `${name}/`, name, additional.rate_limit)
  }
  return windows
}

const appendCodexWindows = (
  windows: UsageWindow[],
  prefix: string,
  feature: string | undefined,
  details: CodexRateLimitDetails | undefined
): void => {
  if (!details) return
  appendCodexWindow(windows, `${prefix}primary`, feature, details.primary_window)
  appendCodexWindow(windows, `${prefix}secondary`, feature, details.secondary_window)
  if (details.limit_reached) {
    windows.push({ name: `${prefix}reached`, used_percent: 100, ...(feature ? { feature } : {}) })
  }
}

const appendCodexWindow = (
  windows: UsageWindow[],
  name: string,
  feature: string | undefined,
  window: CodexLimitWindow | undefined
): void => {
  if (!window) return
  windows.push({
    name,
    used_percent: window.used_percent,
    ...(typeof window.limit_window_seconds === "number"
      ? { limit_window_seconds: window.limit_window_seconds }
      : {}),
    reset_after_seconds: resetAfterSeconds(window),
    ...(feature ? { feature } : {}),
  })
}

const resetAfterSeconds = (window: CodexLimitWindow): number => {
  if (typeof window.reset_after_seconds === "number" && window.reset_after_seconds > 0) {
    return window.reset_after_seconds
  }
  if (typeof window.reset_at !== "number" || window.reset_at <= 0) return 0
  return Math.max(0, window.reset_at - Math.floor(Date.now() / 1000))
}

interface ClaudeUsageResponse {
  readonly five_hour?: ClaudeRateLimit
  readonly seven_day?: ClaudeRateLimit
  readonly seven_day_opus?: ClaudeRateLimit
  readonly seven_day_sonnet?: ClaudeRateLimit
  readonly extra_usage?: {
    readonly is_enabled?: boolean
    readonly utilization?: number
  }
}

interface ClaudeRateLimit {
  readonly utilization?: number
  readonly resets_at?: string
}

const appendClaudeRateLimit = (
  windows: UsageWindow[],
  name: string,
  limit: ClaudeRateLimit | undefined
): void => {
  if (!limit || typeof limit.utilization !== "number") return
  const resetAt = limit.resets_at ? Date.parse(limit.resets_at) : NaN
  windows.push({
    name,
    used_percent: limit.utilization,
    reset_after_seconds: Number.isFinite(resetAt)
      ? Math.max(0, Math.floor((resetAt - Date.now()) / 1000))
      : 0,
  })
}

export const setUpstreamAuthHeaders = (
  headers: Headers,
  account: StoredAccountContract
): Headers => {
  const out = stripSubrouterHeaders(headers)
  out.delete("Authorization")
  out.delete("X-Api-Key")
  out.delete("x-api-key")
  out.delete("ChatGPT-Account-ID")
  const credentials = account.credentials
  if (!credentials) return out
  if (account.kind === "anthropic_apikey" && credentials.apiKey) {
    out.set("x-api-key", credentials.apiKey)
  } else {
    const token = credentials.accessToken ?? credentials.apiKey
    if (token) out.set("Authorization", `Bearer ${token}`)
  }
  if (account.kind === "anthropic_oauth") {
    out.set("Anthropic-Beta", appendCommaValue(out.get("Anthropic-Beta"), "oauth-2025-04-20"))
  }
  if (providerForAccount(account.kind) === "codex" && credentials.accountId) {
    out.set("ChatGPT-Account-ID", credentials.accountId)
  }
  return out
}

export const nextRefreshAt = (
  credentials: AccountCredentials | undefined,
  now = Date.now()
): number | null => {
  if (!credentials?.refreshToken || !credentials.accessToken) return null
  const expiresAt = credentialExpiresAt(credentials)
  if (!expiresAt) return null
  return Math.max(now + 2_000, expiresAt - REFRESH_SKEW_MS)
}

export const credentialExpiresAt = (
  credentials: AccountCredentials | undefined
): number | null => {
  if (!credentials) return null
  if (typeof credentials.expiresAt === "number" && credentials.expiresAt > 0) {
    return credentials.expiresAt
  }
  return jwtExpiryMillis(credentials.accessToken)
}

export const credentialNeedsRefresh = (
  credentials: AccountCredentials | undefined,
  now = Date.now()
): boolean => {
  if (!credentials?.refreshToken || !credentials.accessToken) return false
  const expiresAt = credentialExpiresAt(credentials)
  return expiresAt !== null && now + REFRESH_SKEW_MS >= expiresAt
}

export const refreshOAuthCredentials = async (
  kind: AccountKind,
  credentials: AccountCredentials,
  force = false,
  fetcher: typeof fetch = fetch,
  now = Date.now()
): Promise<OAuthRefreshResult> => {
  if (!isOAuthKind(kind) || !credentials.refreshToken) {
    return { credentials, refreshed: false }
  }
  if (!force && !credentialNeedsRefresh(credentials, now)) {
    return { credentials, refreshed: false }
  }
  if (credentials.refreshFailure && blockingRefreshFailure(credentials.refreshFailure)) {
    throw new Error(credentials.refreshFailure.message)
  }
  if (providerForAccount(kind) === "claude") {
    return refreshClaudeCredentials(credentials, fetcher, now)
  }
  return refreshCodexCredentials(credentials, fetcher, now)
}

const refreshCodexCredentials = async (
  credentials: AccountCredentials,
  fetcher: typeof fetch,
  now: number
): Promise<OAuthRefreshResult> => {
  const response = await fetcher(credentials.tokenEndpoint ?? CODEX_OAUTH_TOKEN_URL, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      client_id: credentials.clientId ?? CODEX_OAUTH_CLIENT_ID,
      grant_type: "refresh_token",
      refresh_token: credentials.refreshToken,
    }),
  })
  const body = await response.text()
  if (!response.ok) {
    throw refreshError(response.status, body)
  }
  const payload = parseRefreshJSON(body)
  if (!payload.access_token || !payload.refresh_token || !payload.id_token) {
    throw new Error("Codex OAuth refresh response missing required fields")
  }
  const { refreshFailure: _refreshFailure, expiresAt: _expiresAt, ...baseCredentials } = credentials
  const expiresAt =
    typeof payload.expires_in === "number" && payload.expires_in > 0
      ? now + payload.expires_in * 1000
      : jwtExpiryMillis(payload.access_token) ?? undefined
  return {
    credentials: {
      ...baseCredentials,
      accessToken: payload.access_token,
      refreshToken: payload.refresh_token,
      idToken: payload.id_token,
      ...(expiresAt !== undefined ? { expiresAt } : {}),
      lastRefreshAt: now,
      credentialGeneration: (credentials.credentialGeneration ?? 0) + 1,
    },
    refreshed: true,
  }
}

const refreshClaudeCredentials = async (
  credentials: AccountCredentials,
  fetcher: typeof fetch,
  now: number
): Promise<OAuthRefreshResult> => {
  const form = new URLSearchParams()
  form.set("client_id", credentials.clientId ?? CLAUDE_OAUTH_CLIENT_ID)
  form.set("grant_type", "refresh_token")
  form.set("refresh_token", credentials.refreshToken ?? "")
  const response = await fetcher(credentials.tokenEndpoint ?? CLAUDE_OAUTH_TOKEN_URL, {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      "anthropic-beta": CLAUDE_OAUTH_BETA,
    },
    body: form,
  })
  const body = await response.text()
  if (!response.ok) {
    throw refreshError(response.status, body)
  }
  const payload = parseRefreshJSON(body)
  if (!payload.access_token) {
    throw new Error("Claude OAuth refresh response missing access_token")
  }
  const { refreshFailure: _refreshFailure, expiresAt: _expiresAt, ...baseCredentials } = credentials
  const expiresAt =
    typeof payload.expires_in === "number" && payload.expires_in > 0
      ? now + payload.expires_in * 1000
      : credentialExpiresAt({ accessToken: payload.access_token }) ?? undefined
  return {
    credentials: {
      ...baseCredentials,
      accessToken: payload.access_token,
      ...(payload.refresh_token || credentials.refreshToken
        ? { refreshToken: payload.refresh_token || credentials.refreshToken }
        : {}),
      ...(expiresAt !== undefined ? { expiresAt } : {}),
      lastRefreshAt: now,
      credentialGeneration: (credentials.credentialGeneration ?? 0) + 1,
    },
    refreshed: true,
  }
}

const parseRefreshJSON = (body: string): Record<string, unknown> & {
  access_token?: string
  refresh_token?: string
  id_token?: string
  expires_in?: number
} => {
  try {
    return JSON.parse(body) as Record<string, unknown>
  } catch {
    // JSON parser errors may quote the provider body. Never let an echoed
    // credential become part of the persisted refresh-failure record.
    throw new Error("OAuth refresh response was not valid JSON")
  }
}

const refreshError = (status: number, body: string): Error => {
  let message = "provider rejected the refresh"
  let providerCode: string | undefined
  let providerType: string | undefined
  try {
    const payload = JSON.parse(body) as {
      error?: { message?: string; code?: string; type?: string } | string
    }
    if (typeof payload.error === "string") {
      providerCode = sanitizedTerminalRefreshCode(payload.error)
      message = providerCode ?? message
    } else if (payload.error) {
      providerCode = payload.error.code
      providerType = payload.error.type
      providerCode =
        sanitizedTerminalRefreshCode(
          providerCode,
          providerType,
          payload.error.message,
        ) ?? providerCode
      message = providerCode ?? providerType ?? message
    }
  } catch {
    // Provider response bodies can contain echoed credentials. Status is enough
    // to diagnose this failure without persisting an untrusted body.
  }
  const error = new Error(`OAuth refresh failed (${status}): ${message}`)
  Object.assign(error, { status, providerCode, providerType })
  return error
}

const sanitizedTerminalRefreshCode = (
  ...values: ReadonlyArray<string | undefined>
): string | undefined => {
  const combined = values.filter(Boolean).join(" ").toLowerCase()
  if (combined.includes("invalid_grant") || combined.includes("invalid grant")) {
    return "invalid_grant"
  }
  if (
    combined.includes("refresh_token") ||
    combined.includes("refresh token")
  ) {
    return "invalid_refresh_token"
  }
  return undefined
}

export const refreshFailureFromError = (
  error: unknown,
  now = Date.now()
): OAuthRefreshFailure => {
  const typed = error as { status?: number; providerCode?: string; providerType?: string; message?: string }
  return {
    at: now,
    status: typed.status ?? 0,
    message: typed.message ?? String(error),
    ...(typed.providerCode ? { providerCode: typed.providerCode } : {}),
    ...(typed.providerType ? { providerType: typed.providerType } : {}),
  }
}

export const terminalRefreshFailure = (failure: OAuthRefreshFailure): boolean => {
  if (failure.status !== 400 && failure.status !== 401) return false
  const combined = `${failure.providerCode ?? ""} ${failure.message}`.toLowerCase()
  return (
    combined.includes("invalid_grant") ||
    combined.includes("invalid grant") ||
    combined.includes("refresh_token") ||
    combined.includes("refresh token")
  )
}

// A transport failure or malformed success response is ambiguous: the provider
// may have rotated the refresh token before the response was lost. Retrying that
// old token can invalidate the only recoverable credential chain, so central
// refresh stops and asks for repair instead of guessing.
export const uncertainRefreshFailure = (failure: OAuthRefreshFailure): boolean =>
  failure.status === 0

export const blockingRefreshFailure = (failure: OAuthRefreshFailure): boolean =>
  terminalRefreshFailure(failure) || uncertainRefreshFailure(failure)

export const jwtExpiryMillis = (token: string | undefined): number | null => {
  if (!token) return null
  const parts = token.split(".")
  if (parts.length < 2) return null
  try {
    const payload = JSON.parse(base64UrlDecode(parts[1]!)) as { exp?: unknown }
    return typeof payload.exp === "number" ? payload.exp * 1000 : null
  } catch {
    return null
  }
}

const base64UrlDecode = (value: string): string => {
  const normalized = value.replaceAll("-", "+").replaceAll("_", "/")
  const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, "=")
  return atob(padded)
}

const appendCommaValue = (existing: string | null, value: string): string => {
  if (!existing) return value
  if (existing.split(",").some((part) => part.trim() === value)) return existing
  return `${existing},${value}`
}

export const upstreamURLForRequest = (
  requestURL: string,
  account: StoredAccountContract,
  defaultBaseUrl: string
): URL => {
  const input = new URL(requestURL)
  const base = new URL(account.credentials?.baseUrl ?? defaultBaseUrl)
  const output = new URL(base.toString())
  output.search = input.search
  output.pathname = upstreamPath(input.pathname, account, base.pathname)
  return output
}

export const redactedUpstreamURL = (url: URL | string): string => {
  const input = new URL(url.toString())
  const query = [...input.searchParams]
    .map(([key]) => `${encodeURIComponent(key)}=[redacted]`)
    .join("&")
  return `${input.origin}${input.pathname}${query ? `?${query}` : ""}`
}

const upstreamPath = (
  requestPath: string,
  account: StoredAccountContract,
  basePath: string
): string => {
  const path = requestPath || "/"
  if (providerForAccount(account.kind) === "claude") return joinPath(basePath, path)
  if (authModeForAccount(account.kind) === "apikey") {
    return path === "/v1" || path.startsWith("/v1/")
      ? joinPath(basePathWithoutTrailingV1(basePath), path)
      : joinPath(basePath, path)
  }
  if (path === "/v1") return joinPath(basePathWithoutTrailingV1(basePath), "/")
  if (path.startsWith("/v1/")) {
    return joinPath(basePathWithoutTrailingV1(basePath), path.slice("/v1".length))
  }
  return joinPath(basePath, path)
}

const basePathWithoutTrailingV1 = (path: string): string =>
  path.replace(/\/v1\/?$/, "") || "/"

const joinPath = (basePath: string, requestPath: string): string => {
  if (!basePath || basePath === "/") return requestPath || "/"
  if (!requestPath || requestPath === "/") return basePath
  return `${basePath.replace(/\/+$/, "")}/${requestPath.replace(/^\/+/, "")}`
}
