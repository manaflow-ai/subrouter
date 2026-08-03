import { afterEach, describe, expect, test } from "bun:test"
import {
  migrateLegacyAccountsToHosted,
  migrateLegacyTenant,
} from "../src/legacy-migration.ts"
import {
  adminToken,
  cleanupWorkers,
  createTenant,
  startWorker,
} from "./helpers/worker.ts"

afterEach(cleanupWorkers)

describe("legacy tenant migration", () => {
  test("moves credentials directly to an allowlisted hosted tenant", async () => {
    const uploads: Array<Record<string, any>> = []
    const migrated = await migrateLegacyAccountsToHosted({
      destinationUrl: "https://sr.cmux.com",
      tenantKey: "srt_0123456789abcdef0123456789abcdef",
      accounts: ["a", "b"].map((suffix) => ({
        id: `legacy-account-${suffix}`,
        kind: "codex_oauth" as const,
        label: "Shared account",
        enabled: true,
        hasTotp: false,
        credentials: {
          accessToken: `legacy-access-${suffix}`,
          refreshToken: `legacy-refresh-${suffix}`,
          idToken: `legacy-id-${suffix}`,
          accountId: `provider-${suffix}`,
        },
      })),
      fetch: (async (input, init) => {
        expect(String(input)).toBe("https://sr.cmux.com/_subrouter/accounts")
        expect(new Headers(init?.headers).get("authorization")).toBe(
          "Bearer srt_0123456789abcdef0123456789abcdef"
        )
        const upload = JSON.parse(String(init?.body)) as Record<string, any>
        uploads.push(upload)
        return Response.json({
          account: {
            id: upload.accountId,
            kind: "codex",
            label: upload.label,
          },
        })
      }) as typeof fetch,
    })

    expect(migrated).toBe(2)
    expect(new Set(uploads.map((upload) => upload.accountId)).size).toBe(2)
    expect(uploads.map((upload) => upload.label)).toEqual([
      "Shared account",
      "Shared account",
    ])
    expect(uploads.map((upload) => upload.tokens?.refreshToken).sort()).toEqual([
      "legacy-refresh-a",
      "legacy-refresh-b",
    ])
  })

  test("quiesces and removes source credentials only after every upload succeeds", async () => {
    let sourceState: "active" | "quiesced" | "removed" = "active"
    const accounts = [legacyAccount("a"), legacyAccount("b")]
    const migrated = await migrateLegacyTenant({
      destinationUrl: "https://sr.cmux.com",
      tenantKey: "srt_0123456789abcdef0123456789abcdef",
      finalizeSource: true,
      source: {
        list: async () => accounts,
        begin: async () => {
          sourceState = "quiesced"
          return accounts
        },
        complete: async () => {
          expect(sourceState).toBe("quiesced")
          sourceState = "removed"
        },
        restore: async () => {
          sourceState = "active"
        },
      },
      fetch: (async (_input, init) => {
        const upload = JSON.parse(String(init?.body)) as Record<string, any>
        return Response.json({ account: { id: upload.accountId } })
      }) as typeof fetch,
    })

    expect(migrated).toBe(2)
    expect(sourceState).toBe("removed")
  })

  test("restores source routing when a finalized migration upload fails", async () => {
    let sourceState: "active" | "quiesced" | "removed" = "active"
    await expect(
      migrateLegacyTenant({
        destinationUrl: "https://sr.cmux.com",
        tenantKey: "srt_0123456789abcdef0123456789abcdef",
        finalizeSource: true,
        source: {
          list: async () => [legacyAccount("a")],
          begin: async () => {
            sourceState = "quiesced"
            return [legacyAccount("a")]
          },
          complete: async () => {
            sourceState = "removed"
          },
          restore: async () => {
            sourceState = "active"
          },
        },
        fetch: (async () => {
          throw new Error("network body with secret")
        }) as typeof fetch,
      })
    ).rejects.toThrow("destination is unavailable")
    expect(sourceState).toBe("active")
  })

  test("admin route rejects untrusted destinations without exposing source credentials", async () => {
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Legacy team")
    const secret = "legacy-refresh-secret"
    const upload = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${tenant.key}`,
        "content-type": "application/json",
      },
      body: JSON.stringify({
        provider: "codex",
        label: "Shared account",
        tokens: {
          accessToken: "legacy-access-secret",
          refreshToken: secret,
          idToken: "legacy-id-secret",
          accountID: "provider-account",
        },
      }),
    })
    expect(upload.status).toBe(200)

    const response = await fetch(
      `${worker.baseURL}/admin/tenants/${tenant.id}/migrate-hosted`,
      {
        method: "POST",
        headers: {
          authorization: `Bearer ${adminToken}`,
          "content-type": "application/json",
        },
        body: JSON.stringify({
          destinationUrl: "https://attacker.example",
          tenantKey: "srt_0123456789abcdef0123456789abcdef",
        }),
      }
    )
    expect(response.status).toBe(409)
    const responseText = await response.text()
    expect(responseText).toContain("destination is not allowed")
    expect(responseText).not.toContain(secret)
    expect(responseText).not.toContain(tenant.key)
  }, 60_000)
})

const legacyAccount = (suffix: string) => ({
  id: `legacy-account-${suffix}`,
  kind: "codex_oauth" as const,
  label: "Shared account",
  enabled: true,
  hasTotp: false,
  credentials: {
    accessToken: `legacy-access-${suffix}`,
    refreshToken: `legacy-refresh-${suffix}`,
    idToken: `legacy-id-${suffix}`,
    accountId: `provider-${suffix}`,
  },
})
