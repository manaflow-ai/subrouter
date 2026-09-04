import { Context, Effect, Layer } from "effect"

export interface Account {
  readonly id: string
  readonly orgId: string
  readonly kind: "codex_oauth" | "anthropic_oauth" | "openai_apikey" | "anthropic_apikey"
  readonly label: string
  readonly enabled: boolean
  readonly rateLimitRemaining?: number
  readonly modelQuotas?: AccountModelQuotas
  readonly lastUsedAt?: number
}

export interface AccountModelQuota {
  readonly remainingPercent: number
  readonly resetsAt?: number
  readonly protectedBelowPercent?: number
}

export type AccountModelQuotas = Readonly<Record<string, AccountModelQuota>>

// Named model-family quota pools. A request whose model name contains one of
// these keywords draws from that pool instead of the account-wide "default".
const MODEL_QUOTA_POOLS = ["spark", "opus", "sonnet"] as const

export const quotaKeyForModel = (model: string | undefined): string => {
  const normalized = model?.trim().toLowerCase()
  if (!normalized) return "default"
  for (const pool of MODEL_QUOTA_POOLS) {
    if (normalized.includes(pool)) return pool
  }
  return "default"
}

export const accountHasQuotaForModel = (
  account: Account,
  quotaKey: string
): boolean => {
  const quota = account.modelQuotas?.[quotaKey]
  if (!quota) return quotaKey === "default"
  return quota.remainingPercent > 0
}

export interface AccountStore {
  readonly list: (orgId: string) => Effect.Effect<ReadonlyArray<Account>>
  readonly pick: (input: {
    readonly orgId: string
    readonly sessionId: string
    readonly preferAccountId?: string
    readonly model?: string
    readonly quotaKey?: string
  }) => Effect.Effect<Account | null>
  readonly recordUse: (accountId: string) => Effect.Effect<void>
}

export class AccountStoreTag extends Context.Tag("AccountStore")<
  AccountStoreTag,
  AccountStore
>() {}

/**
 * Sticky session table. In production: a Postgres row per (orgId, sessionId).
 * Here: in-memory Map. Stickiness ensures cached agent context stays useful
 * (matches Go subrouter's `X-Subrouter-Session` semantics).
 */
export const makeInMemoryStickyStore = () => {
  const map = new Map<string, string>() // `${orgId}:${sessionId}` -> accountId
  return {
    get: (orgId: string, sessionId: string, quotaKey = "default"): string | null =>
      map.get(`${orgId}:${quotaKey}:${sessionId}`) ?? null,
    set: (
      orgId: string,
      sessionId: string,
      accountId: string,
      quotaKey = "default"
    ): void => {
      map.set(`${orgId}:${quotaKey}:${sessionId}`, accountId)
    },
    clear: (): void => {
      map.clear()
    },
  }
}

/**
 * Reference in-memory AccountStore impl that round-robins enabled accounts and
 * respects sticky sessions. Production swaps in a Postgres-backed implementation
 * that reads from `accounts` and `subrouter_session_assignments`.
 */
export const makeInMemoryAccountStoreLayer = (initial: ReadonlyArray<Account>) => {
  const accounts = [...initial]
  const sticky = makeInMemoryStickyStore()
  let cursor = 0

  return Layer.succeed(AccountStoreTag, {
    list: (orgId) =>
      Effect.succeed(accounts.filter((a) => a.orgId === orgId && a.enabled)),

    pick: ({ orgId, sessionId, preferAccountId, model, quotaKey }) => {
      const resolvedQuotaKey = quotaKey ?? quotaKeyForModel(model)
      const isEligible = (account: Account): boolean =>
        account.orgId === orgId &&
        account.enabled &&
        accountHasQuotaForModel(account, resolvedQuotaKey)

      // 1. sticky table first
      const stickyId = sticky.get(orgId, sessionId, resolvedQuotaKey)
      if (stickyId) {
        const found = accounts.find((a) => a.id === stickyId && isEligible(a))
        if (found) return Effect.succeed(found)
      }
      // 2. explicit pin
      if (preferAccountId) {
        const found = accounts.find(
          (a) => a.id === preferAccountId && isEligible(a)
        )
        if (found) {
          sticky.set(orgId, sessionId, found.id, resolvedQuotaKey)
          return Effect.succeed(found)
        }
      }
      // 3. round-robin over enabled accounts for the org
      const eligible = accounts.filter(isEligible)
      if (eligible.length === 0) return Effect.succeed(null)
      const pick = eligible[cursor % eligible.length]!
      cursor += 1
      sticky.set(orgId, sessionId, pick.id, resolvedQuotaKey)
      return Effect.succeed(pick)
    },

    recordUse: (accountId) =>
      Effect.sync(() => {
        const i = accounts.findIndex((a) => a.id === accountId)
        if (i >= 0) {
          accounts[i] = { ...accounts[i]!, lastUsedAt: Date.now() }
        }
      }),
  } satisfies AccountStore)
}
