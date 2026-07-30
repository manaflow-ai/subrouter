import { describe, expect, test } from "bun:test"
import { Effect } from "effect"
import {
  AccountStoreTag,
  makeInMemoryAccountStoreLayer,
  type Account,
} from "../src/index.ts"

const seed: ReadonlyArray<Account> = [
  { id: "a1", orgId: "org-1", kind: "codex_oauth", label: "team-1", enabled: true },
  { id: "a2", orgId: "org-1", kind: "codex_oauth", label: "team-2", enabled: true },
  { id: "a3", orgId: "org-1", kind: "codex_oauth", label: "disabled", enabled: false },
  { id: "b1", orgId: "org-2", kind: "openai_apikey", label: "other-org", enabled: true },
]

describe("SubrouterActor sticky routing", () => {
  test("same session picks the same account twice in a row", async () => {
    const layer = makeInMemoryAccountStoreLayer(seed)
    const program = Effect.gen(function* () {
      const store = yield* AccountStoreTag
      const first = yield* store.pick({ orgId: "org-1", sessionId: "s1" })
      const second = yield* store.pick({ orgId: "org-1", sessionId: "s1" })
      return [first?.id, second?.id]
    })
    const [a, b] = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(a).toBeTruthy()
    expect(a).toBe(b)
  })

  test("round-robin distributes new sessions across enabled accounts", async () => {
    const layer = makeInMemoryAccountStoreLayer(seed)
    const program = Effect.gen(function* () {
      const store = yield* AccountStoreTag
      const out: Array<string | null> = []
      for (let i = 0; i < 4; i++) {
        const picked = yield* store.pick({
          orgId: "org-1",
          sessionId: `new-${i}`,
        })
        out.push(picked?.id ?? null)
      }
      return out
    })
    const result = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    // 2 enabled accounts in org-1, so 4 sessions should hit each twice.
    const counts = result.reduce<Record<string, number>>((acc, id) => {
      if (!id) return acc
      acc[id] = (acc[id] ?? 0) + 1
      return acc
    }, {})
    expect(Object.keys(counts).sort()).toEqual(["a1", "a2"])
    expect(counts["a1"]).toBe(2)
    expect(counts["a2"]).toBe(2)
  })

  test("preferAccountId pins to a specific account and starts sticky", async () => {
    const layer = makeInMemoryAccountStoreLayer(seed)
    const program = Effect.gen(function* () {
      const store = yield* AccountStoreTag
      const first = yield* store.pick({
        orgId: "org-1",
        sessionId: "pinned",
        preferAccountId: "a2",
      })
      const second = yield* store.pick({ orgId: "org-1", sessionId: "pinned" })
      return [first?.id, second?.id]
    })
    const [a, b] = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(a).toBe("a2")
    expect(b).toBe("a2")
  })

  test("org isolation: org-1 sessions never get org-2 accounts", async () => {
    const layer = makeInMemoryAccountStoreLayer(seed)
    const program = Effect.gen(function* () {
      const store = yield* AccountStoreTag
      const out: Array<string | null> = []
      for (let i = 0; i < 5; i++) {
        const picked = yield* store.pick({
          orgId: "org-1",
          sessionId: `iso-${i}`,
        })
        out.push(picked?.id ?? null)
      }
      return out
    })
    const result = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    for (const id of result) {
      expect(id).not.toBe("b1")
      expect(id).not.toBe("a3") // disabled
    }
  })

  test("no eligible accounts -> pick returns null", async () => {
    const layer = makeInMemoryAccountStoreLayer([])
    const program = Effect.gen(function* () {
      const store = yield* AccountStoreTag
      return yield* store.pick({ orgId: "org-x", sessionId: "s" })
    })
    const result = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(result).toBeNull()
  })

  test("spark model uses spark quota even when default quota is empty", async () => {
    const layer = makeInMemoryAccountStoreLayer([
      {
        id: "regular-cooked",
        orgId: "org-spark",
        kind: "codex_oauth",
        label: "regular cooked",
        enabled: true,
        modelQuotas: {
          default: { remainingPercent: 0 },
          spark: { remainingPercent: 100 },
        },
      },
    ])
    const program = Effect.gen(function* () {
      const store = yield* AccountStoreTag
      return yield* store.pick({
        orgId: "org-spark",
        sessionId: "spark-session",
        model: "GPT-5.3-Codex-Spark",
      })
    })
    const result = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(result?.id).toBe("regular-cooked")
  })
})

describe("dual-provider model-family quota routing", () => {
  test("claude opus model routes to a claude account carrying an opus pool", async () => {
    const layer = makeInMemoryAccountStoreLayer([
      {
        id: "codex-1",
        orgId: "org-both",
        kind: "codex_oauth",
        label: "codex",
        enabled: true,
        modelQuotas: {
          default: { remainingPercent: 100 },
          spark: { remainingPercent: 100 },
        },
      },
      {
        id: "claude-1",
        orgId: "org-both",
        kind: "anthropic_oauth",
        label: "claude",
        enabled: true,
        modelQuotas: {
          default: { remainingPercent: 100 },
          opus: { remainingPercent: 100 },
          sonnet: { remainingPercent: 100 },
        },
      },
    ])
    const program = Effect.gen(function* () {
      const store = yield* AccountStoreTag
      return yield* store.pick({
        orgId: "org-both",
        sessionId: "s-opus",
        model: "claude-opus-4-1",
      })
    })
    const result = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(result?.id).toBe("claude-1")
  })

  test("claude sonnet model uses the sonnet pool even when opus is exhausted", async () => {
    const layer = makeInMemoryAccountStoreLayer([
      {
        id: "claude-1",
        orgId: "org-c",
        kind: "anthropic_oauth",
        label: "claude",
        enabled: true,
        modelQuotas: {
          default: { remainingPercent: 100 },
          opus: { remainingPercent: 0 },
          sonnet: { remainingPercent: 100 },
        },
      },
    ])
    const program = Effect.gen(function* () {
      const store = yield* AccountStoreTag
      return yield* store.pick({
        orgId: "org-c",
        sessionId: "s-sonnet",
        model: "claude-sonnet-4-5-20250929",
      })
    })
    const result = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(result?.id).toBe("claude-1")
  })

  test("provider isolation: a claude opus model never selects a codex account", async () => {
    const layer = makeInMemoryAccountStoreLayer([
      {
        id: "codex-only",
        orgId: "org-codex",
        kind: "codex_oauth",
        label: "codex",
        enabled: true,
        modelQuotas: {
          default: { remainingPercent: 100 },
          spark: { remainingPercent: 100 },
        },
      },
    ])
    const program = Effect.gen(function* () {
      const store = yield* AccountStoreTag
      return yield* store.pick({
        orgId: "org-codex",
        sessionId: "s-x",
        model: "claude-opus-4-1",
      })
    })
    const result = await Effect.runPromise(program.pipe(Effect.provide(layer)))
    expect(result).toBeNull()
  })
})
