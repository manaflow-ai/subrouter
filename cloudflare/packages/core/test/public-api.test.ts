import { expect, test } from "bun:test"
import { Effect, Layer } from "effect"
import * as core from "../src/index.ts"
import * as service from "../src/service.ts"

test("the core barrel exposes one coherent service and account-store API", async () => {
  expect(core.SubrouterService).toBe(service.SubrouterService)
  expect(core.NoEligibleAccount).toBe(service.NoEligibleAccount)
  expect(typeof core.makeInMemoryAccountStoreLayer).toBe("function")

  const layer = core.makeInMemoryAccountStoreLayer([
    {
      id: "account-1",
      orgId: "org-1",
      kind: "codex_oauth",
      label: "primary",
      enabled: true,
    },
  ])
  const program = Effect.gen(function* () {
    const subrouter = yield* core.SubrouterService
    return yield* subrouter.route({ orgId: "org-1", sessionId: "session-1" })
  }).pipe(Effect.provide(core.makeSubrouterServiceLayer().pipe(
    Layer.provideMerge(layer)
  )))

  const result = await Effect.runPromise(program)
  expect(result.account.id).toBe("account-1")
})
