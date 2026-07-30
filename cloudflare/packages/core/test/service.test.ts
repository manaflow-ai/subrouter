import { describe, expect, test } from "bun:test"
import { Cause, Effect, Exit, Layer } from "effect"
import {
  NoEligibleAccount,
  SubrouterService,
  makeInMemoryAccountStoreLayer,
  makeSubrouterServiceLayer,
  type Account,
} from "../src/index.ts"

const seed: ReadonlyArray<Account> = [
  { id: "a1", orgId: "org-1", kind: "codex_oauth", label: "primary", enabled: true },
  { id: "a2", orgId: "org-1", kind: "codex_oauth", label: "spare", enabled: true },
]

const buildLayer = () =>
  makeSubrouterServiceLayer().pipe(
    Layer.provideMerge(makeInMemoryAccountStoreLayer(seed))
  )

describe("SubrouterService.route", () => {
  test("happy path: returns a sticky account from the org pool", async () => {
    const layer = buildLayer()
    const program = Effect.gen(function* () {
      const sub = yield* SubrouterService
      return yield* sub.route({ orgId: "org-1", sessionId: "sess-1" })
    })
    const out = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(out.account.orgId).toBe("org-1")
    expect(["a1", "a2"]).toContain(out.account.id)
  })

  test("sticky: two route calls for same session pick the same account", async () => {
    const layer = buildLayer()
    const program = Effect.gen(function* () {
      const sub = yield* SubrouterService
      const a = yield* sub.route({ orgId: "org-1", sessionId: "sess-sticky" })
      const b = yield* sub.route({ orgId: "org-1", sessionId: "sess-sticky" })
      return [a.account.id, b.account.id]
    })
    const [a, b] = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(a).toBe(b)
  })

  test("preferAccountId pins to that account when eligible", async () => {
    const layer = buildLayer()
    const program = Effect.gen(function* () {
      const sub = yield* SubrouterService
      return yield* sub.route({
        orgId: "org-1",
        sessionId: "pinned",
        preferAccountId: "a2",
      })
    })
    const out = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(out.account.id).toBe("a2")
  })

  test("no eligible account for the org -> NoEligibleAccount", async () => {
    const layer = buildLayer()
    const program = Effect.gen(function* () {
      const sub = yield* SubrouterService
      return yield* sub.route({ orgId: "unknown-org", sessionId: "sess" })
    })
    const exit = await Effect.runPromiseExit(program.pipe(Effect.provide(layer)))
    expect(Exit.isFailure(exit)).toBe(true)
    if (!Exit.isFailure(exit)) return
    const failure = Cause.failureOption(exit.cause)
    if (failure._tag !== "Some") return
    expect(failure.value).toBeInstanceOf(NoEligibleAccount)
  })

  test("recordUsage is a no-throw observability hook", async () => {
    const layer = buildLayer()
    const program = Effect.gen(function* () {
      const sub = yield* SubrouterService
      const r = yield* sub.route({ orgId: "org-1", sessionId: "sess-rec" })
      yield* sub.recordUsage({
        orgId: "org-1",
        sessionId: "sess-rec",
        accountId: r.account.id,
      })
      return r.account.id
    })
    const id = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(["a1", "a2"]).toContain(id)
  })
})
