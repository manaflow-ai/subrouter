/**
 * SubrouterService — the single entry point the AI proxy hits on every
 * upstream request. Picks a sticky account for the (orgId, sessionId)
 * tuple.
 *
 * Flow:
 *   1. route({orgId, sessionId, preferAccountId?}) -> Account
 *   2. recordUsage({...}) -> updates last-used for observability
 *
 * Budget enforcement was intentionally removed — handled higher up if at
 * all. The subrouter just routes; it does not gate.
 */
import { Context, Effect, Layer } from "effect"
import { AccountStoreTag, type Account } from "./account-store.ts"

export class NoEligibleAccount extends Error {
  readonly _tag = "NoEligibleAccount"
  constructor(public readonly orgId: string) {
    super(`no eligible subrouter account for org ${orgId}`)
  }
}

export interface PickedRoute {
  readonly account: Account
}

export class SubrouterService extends Context.Tag("SubrouterService")<
  SubrouterService,
  {
    readonly route: (input: {
      readonly orgId: string
      readonly sessionId: string
      readonly preferAccountId?: string
      readonly model?: string
      readonly quotaKey?: string
    }) => Effect.Effect<PickedRoute, NoEligibleAccount>

    /** Optional observability hook; no enforcement. */
    readonly recordUsage: (input: {
      readonly orgId: string
      readonly sessionId: string
      readonly accountId: string
    }) => Effect.Effect<void>
  }
>() {}

export const makeSubrouterServiceLayer = () =>
  Layer.effect(
    SubrouterService,
    Effect.gen(function* () {
      const accounts = yield* AccountStoreTag
      return {
        route: ({ orgId, sessionId, preferAccountId, model, quotaKey }) =>
          Effect.gen(function* () {
            const pickArgs: Parameters<typeof accounts.pick>[0] = {
              orgId,
              sessionId,
            }
            if (preferAccountId !== undefined) {
              ;(pickArgs as { preferAccountId?: string }).preferAccountId =
                preferAccountId
            }
            if (model !== undefined) {
              ;(pickArgs as { model?: string }).model = model
            }
            if (quotaKey !== undefined) {
              ;(pickArgs as { quotaKey?: string }).quotaKey = quotaKey
            }
            const account = yield* accounts.pick(pickArgs)
            if (!account) {
              return yield* Effect.fail(new NoEligibleAccount(orgId))
            }
            yield* accounts.recordUse(account.id)
            return { account }
          }),

        recordUsage: ({ accountId }) => accounts.recordUse(accountId),
      }
    })
  )
