import type { AccountCredentials, AccountKind } from "./contract.ts"

export interface LegacyMigrationAccount {
  readonly id: string
  readonly kind: AccountKind
  readonly label: string
  readonly enabled: boolean
  readonly hasTotp: boolean
  readonly updatedAt?: number
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
  readonly markActivating?: () => Promise<void>
}

const productionOrigins = new Set([
  "https://sr.cmux.com",
  "https://sr.cmux.dev",
])
const tenantKeyPattern = /^srt_[0-9a-f]{32}$/
const migrationIdPattern = /^[a-z0-9][a-z0-9._-]{0,159}$/
const maxMigrationAccounts = 16

export async function migrateLegacyTenant(options: {
  readonly destinationUrl: unknown
  readonly tenantKey: unknown
  readonly finalizeSource: boolean
  readonly allowLoopback?: boolean
  readonly migrationId?: string
  readonly source: LegacyMigrationSource
  readonly fetch?: typeof fetch
}): Promise<number> {
  // Reject operator input before quiescing any source credential.
  const destinationUrl = migrationDestination(
    options.destinationUrl,
    options.allowLoopback ?? false
  )
  const tenantKey = migrationTenantKey(options.tenantKey)
  const migrationId = normalizedMigrationID(options.migrationId)
  let accounts: ReadonlyArray<LegacyMigrationAccount> = []
  let sourceBegan = false
  let destinationAttempted = false
  let sourceCompleted = false
  try {
    if (options.finalizeSource) {
      // Mark the attempt before calling begin so a source with persisted
      // recovery state can undo a failure that happens after quiescing.
      sourceBegan = true
      accounts = await options.source.begin()
    } else {
      accounts = await options.source.list()
    }
    destinationAttempted = true
    const migrated = await migrateLegacyAccountsToHosted({
      destinationUrl,
      tenantKey,
      migrationId,
      accounts,
      allowLoopback: options.allowLoopback,
      fetch: options.fetch,
    })
    if (sourceBegan) {
      await options.source.markActivating?.()
      await activateLegacyMigrationDestination({
        destinationUrl,
        tenantKey,
        migrationId,
        accountIds: accounts.map((account) => account.id),
        allowLoopback: options.allowLoopback,
        fetch: options.fetch,
      })
      await options.source.complete(accounts)
      sourceCompleted = true
    }
    return migrated
  } catch (error) {
    if (destinationAttempted && !sourceCompleted) {
      try {
        await rollbackLegacyMigrationDestination({
          destinationUrl,
          tenantKey,
          migrationId,
          allowLoopback: options.allowLoopback,
          fetch: options.fetch,
        })
      } catch {
        throw new Error("hosted migration failed and destination rollback failed")
      }
    }
    if (sourceBegan && !sourceCompleted) {
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
  readonly migrationId?: string
  readonly accounts: ReadonlyArray<LegacyMigrationAccount>
  readonly allowLoopback?: boolean
  readonly fetch?: typeof fetch
}): Promise<number> {
  const destination = migrationDestination(
    options.destinationUrl,
    options.allowLoopback ?? false
  )
  const tenantKey = migrationTenantKey(options.tenantKey)
  const migrationId = normalizedMigrationID(options.migrationId)
  if (options.accounts.length > maxMigrationAccounts) {
    throw new Error("hosted migration account limit exceeded")
  }

  // Validate every source row before the first external mutation. A retry can
  // safely overwrite the same destination ids after a network interruption.
  const uploads = options.accounts.map(migrationUpload)
  const accountIds = uploads.map((upload) => String(upload.accountId))
  const body = await migrationDestinationRequest({
    destination,
    tenantKey,
    path: "/_subrouter/accounts/migration/stage",
    body: { migrationId, accounts: uploads },
    fetch: options.fetch,
    operation: "stage",
  })
  if (
    body["ok"] !== true ||
    !Array.isArray(body["accountIds"]) ||
    body["accountIds"].length !== accountIds.length ||
    body["accountIds"].some(
      (accountId, index) => accountId !== accountIds[index]
    )
  ) {
    throw new Error("hosted migration returned an invalid staged batch")
  }
  return uploads.length
}

async function activateLegacyMigrationDestination(options: {
  readonly destinationUrl: unknown
  readonly tenantKey: unknown
  readonly migrationId?: string
  readonly accountIds: ReadonlyArray<string>
  readonly allowLoopback?: boolean
  readonly fetch?: typeof fetch
}): Promise<void> {
  const body = await migrationDestinationRequest({
    destination: migrationDestination(
      options.destinationUrl,
      options.allowLoopback ?? false
    ),
    tenantKey: migrationTenantKey(options.tenantKey),
    path: "/_subrouter/accounts/migration/activate",
    body: {
      migrationId: normalizedMigrationID(options.migrationId),
      accountIds: options.accountIds,
    },
    fetch: options.fetch,
    operation: "activation",
  })
  if (body["ok"] !== true) {
    throw new Error("hosted migration returned an invalid activation")
  }
}

export async function rollbackLegacyMigrationDestination(options: {
  readonly destinationUrl: unknown
  readonly tenantKey: unknown
  readonly migrationId?: string
  readonly allowLoopback?: boolean
  readonly fetch?: typeof fetch
}): Promise<void> {
  const body = await migrationDestinationRequest({
    destination: migrationDestination(
      options.destinationUrl,
      options.allowLoopback ?? false
    ),
    tenantKey: migrationTenantKey(options.tenantKey),
    path: "/_subrouter/accounts/migration/rollback",
    body: { migrationId: normalizedMigrationID(options.migrationId) },
    fetch: options.fetch,
    operation: "rollback",
  })
  if (body["ok"] !== true) {
    throw new Error("hosted migration returned an invalid rollback")
  }
}

async function migrationDestinationRequest(options: {
  readonly destination: string
  readonly tenantKey: string
  readonly path: string
  readonly body: Record<string, unknown>
  readonly fetch?: typeof fetch
  readonly operation: string
}): Promise<Record<string, unknown>> {
  let response: Response
  try {
    response = await (options.fetch ?? fetch)(
      `${options.destination}${options.path}`,
      {
        method: "POST",
        redirect: "manual",
        signal: AbortSignal.timeout(10_000),
        headers: {
          authorization: `Bearer ${options.tenantKey}`,
          "content-type": "application/json",
        },
        body: JSON.stringify(options.body),
      }
    )
  } catch {
    throw new Error("hosted migration destination is unavailable")
  }
  if (!response.ok) {
    throw new Error(
      `hosted migration ${options.operation} failed (${response.status})`
    )
  }
  const body = await response.json().catch(() => null)
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    throw new Error("hosted migration returned an invalid response")
  }
  return body as Record<string, unknown>
}

function normalizedMigrationID(value: string | undefined): string {
  const migrationId = value ?? "legacy-migration"
  if (!migrationIdPattern.test(migrationId)) {
    throw new Error("hosted migration id is invalid")
  }
  return migrationId
}

function migrationTenantKey(value: unknown): string {
  if (typeof value !== "string" || !tenantKeyPattern.test(value)) {
    throw new Error("hosted migration tenant key is invalid")
  }
  return value
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
