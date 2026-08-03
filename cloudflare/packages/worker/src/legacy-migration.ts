import type { AccountCredentials, AccountKind } from "./contract.ts"

export interface LegacyMigrationAccount {
  readonly id: string
  readonly kind: AccountKind
  readonly label: string
  readonly enabled: boolean
  readonly hasTotp: boolean
  readonly credentials?: AccountCredentials
}

export interface LegacyMigrationSource {
  readonly list: () => Promise<ReadonlyArray<LegacyMigrationAccount>>
  readonly begin: () => Promise<ReadonlyArray<LegacyMigrationAccount>>
  readonly complete: (
    accounts: ReadonlyArray<LegacyMigrationAccount>
  ) => Promise<void>
  readonly restore: (
    accounts: ReadonlyArray<LegacyMigrationAccount>
  ) => Promise<void>
}

const productionOrigins = new Set([
  "https://sr.cmux.com",
  "https://sr.cmux.dev",
])
const tenantKeyPattern = /^srt_[0-9a-f]{32}$/
const maxMigrationAccounts = 256

export async function migrateLegacyTenant(options: {
  readonly destinationUrl: unknown
  readonly tenantKey: unknown
  readonly finalizeSource: boolean
  readonly allowLoopback?: boolean
  readonly source: LegacyMigrationSource
  readonly fetch?: typeof fetch
}): Promise<number> {
  const accounts = options.finalizeSource
    ? await options.source.begin()
    : await options.source.list()
  try {
    const migrated = await migrateLegacyAccountsToHosted({
      destinationUrl: options.destinationUrl,
      tenantKey: options.tenantKey,
      accounts,
      allowLoopback: options.allowLoopback,
      fetch: options.fetch,
    })
    if (options.finalizeSource) {
      await options.source.complete(accounts)
    }
    return migrated
  } catch (error) {
    if (options.finalizeSource) {
      try {
        await options.source.restore(accounts)
      } catch {
        throw new Error("hosted migration failed and source restoration failed")
      }
    }
    throw error
  }
}

export async function migrateLegacyAccountsToHosted(options: {
  readonly destinationUrl: unknown
  readonly tenantKey: unknown
  readonly accounts: ReadonlyArray<LegacyMigrationAccount>
  readonly allowLoopback?: boolean
  readonly fetch?: typeof fetch
}): Promise<number> {
  const destination = migrationDestination(
    options.destinationUrl,
    options.allowLoopback ?? false
  )
  if (
    typeof options.tenantKey !== "string" ||
    !tenantKeyPattern.test(options.tenantKey)
  ) {
    throw new Error("hosted migration tenant key is invalid")
  }
  if (options.accounts.length > maxMigrationAccounts) {
    throw new Error("hosted migration account limit exceeded")
  }

  // Validate every source row before the first external mutation. A retry can
  // safely overwrite the same destination ids after a network interruption.
  const uploads = options.accounts.map(migrationUpload)
  const fetchImpl = options.fetch ?? fetch
  for (const upload of uploads) {
    let response: Response
    try {
      response = await fetchImpl(`${destination}/_subrouter/accounts`, {
        method: "POST",
        redirect: "manual",
        signal: AbortSignal.timeout(10_000),
        headers: {
          authorization: `Bearer ${options.tenantKey}`,
          "content-type": "application/json",
        },
        body: JSON.stringify(upload),
      })
    } catch {
      throw new Error("hosted migration destination is unavailable")
    }
    if (!response.ok) {
      throw new Error(`hosted migration upload failed (${response.status})`)
    }
    const body = await response.json().catch(() => null)
    if (
      !body ||
      typeof body !== "object" ||
      !("account" in body) ||
      !body.account ||
      typeof body.account !== "object" ||
      !("id" in body.account) ||
      body.account.id !== upload.accountId
    ) {
      throw new Error("hosted migration returned an invalid account")
    }
  }
  return uploads.length
}

function migrationDestination(value: unknown, allowLoopback: boolean): string {
  if (typeof value !== "string") {
    throw new Error("hosted migration destination is invalid")
  }
  let url: URL
  try {
    url = new URL(value)
  } catch {
    throw new Error("hosted migration destination is invalid")
  }
  if (
    url.username ||
    url.password ||
    url.pathname !== "/" ||
    url.search ||
    url.hash
  ) {
    throw new Error("hosted migration destination is invalid")
  }
  const origin = url.origin
  if (productionOrigins.has(origin)) return origin
  if (
    allowLoopback &&
    url.protocol === "http:" &&
    (url.hostname === "localhost" ||
      url.hostname === "127.0.0.1" ||
      url.hostname === "[::1]")
  ) {
    return origin
  }
  throw new Error("hosted migration destination is not allowed")
}

function migrationUpload(account: LegacyMigrationAccount): Record<string, unknown> {
  const id = normalizedString(account.id, 320)
  const label = normalizedString(account.label, 320)
  if (!id || !label || !account.enabled || account.hasTotp) {
    throw new Error("legacy account cannot be migrated safely")
  }
  const credentials = account.credentials
  if (!credentials) {
    throw new Error("legacy account credentials are unavailable")
  }
  switch (account.kind) {
    case "codex_oauth": {
      const accessToken = requiredSecret(credentials.accessToken)
      const refreshToken = requiredSecret(credentials.refreshToken)
      const idToken = requiredSecret(credentials.idToken)
      const accountID = requiredSecret(credentials.accountId)
      return {
        provider: "codex",
        accountId: id,
        label,
        tokens: { accessToken, refreshToken, idToken, accountID },
      }
    }
    case "openai_apikey":
      return {
        provider: "openai-apikey",
        accountId: id,
        label,
        apiKey: requiredSecret(credentials.apiKey),
      }
    case "anthropic_apikey":
      return {
        provider: "anthropic-apikey",
        accountId: id,
        label,
        apiKey: requiredSecret(credentials.apiKey),
      }
    case "anthropic_oauth":
      throw new Error("legacy Claude OAuth migration is not supported")
  }
}

function normalizedString(value: unknown, maxLength: number): string | null {
  if (typeof value !== "string") return null
  const normalized = value.trim()
  return normalized && normalized.length <= maxLength ? normalized : null
}

function requiredSecret(value: unknown): string {
  const normalized = normalizedString(value, 64 * 1024)
  if (!normalized) throw new Error("legacy account credentials are incomplete")
  return normalized
}
