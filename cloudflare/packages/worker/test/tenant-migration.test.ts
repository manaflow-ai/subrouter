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

const servers: Array<{ stop: () => void }> = []

afterEach(async () => {
  await cleanupWorkers()
  for (const server of servers.splice(0)) server.stop()
})

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
    const sourceState = {
      value: "active" as "active" | "quiesced" | "removed",
    }
    const accounts = [legacyAccount("a"), legacyAccount("b")]
    const migrated = await migrateLegacyTenant({
      destinationUrl: "https://sr.cmux.com",
      tenantKey: "srt_0123456789abcdef0123456789abcdef",
      finalizeSource: true,
      source: {
        list: async () => accounts,
        begin: async () => {
          sourceState.value = "quiesced"
          return accounts
        },
        complete: async () => {
          expect(sourceState.value).toBe("quiesced")
          sourceState.value = "removed"
        },
        restore: async () => {
          sourceState.value = "active"
        },
      },
      fetch: (async (_input, init) => {
        const upload = JSON.parse(String(init?.body)) as Record<string, any>
        return Response.json({ account: { id: upload.accountId } })
      }) as typeof fetch,
    })

    expect(migrated).toBe(2)
    expect(sourceState.value).toBe("removed")
  })

  test("restores source routing when a finalized migration upload fails", async () => {
    const sourceState = {
      value: "active" as "active" | "quiesced" | "removed",
    }
    await expect(
      migrateLegacyTenant({
        destinationUrl: "https://sr.cmux.com",
        tenantKey: "srt_0123456789abcdef0123456789abcdef",
        finalizeSource: true,
        source: {
          list: async () => [legacyAccount("a")],
          begin: async () => {
            sourceState.value = "quiesced"
            return [legacyAccount("a")]
          },
          complete: async () => {
            sourceState.value = "removed"
          },
          restore: async () => {
            sourceState.value = "active"
          },
        },
        fetch: Object.assign(
          async () => {
            throw new Error("network body with secret")
          },
          { preconnect() {} }
        ) as typeof fetch,
      })
    ).rejects.toThrow("destination is unavailable")
    expect(sourceState.value).toBe("active")
  })

  test("finalizes source rows through the admin route after hosted upload", async () => {
    const uploads: Array<Record<string, any>> = []
    const destination = Bun.serve({
      port: 0,
      async fetch(request) {
        const upload = (await request.json()) as Record<string, any>
        uploads.push(upload)
        return Response.json({ account: { id: upload.accountId } })
      },
    })
    servers.push(destination)
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Finalized team")
    const secret = "legacy-finalization-refresh-secret"
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
          accessToken: "legacy-finalization-access",
          refreshToken: secret,
          idToken: "legacy-finalization-id",
          accountID: "legacy-provider-account",
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
          destinationUrl: destination.url.origin,
          tenantKey: "srt_0123456789abcdef0123456789abcdef",
          finalizeSource: true,
        }),
      }
    )
    const responseText = await response.text()
    expect({ status: response.status, body: responseText }).toEqual({
      status: 200,
      body: JSON.stringify({
        ok: true,
        migrated: 1,
        sourceFinalized: true,
      }),
    })
    expect(JSON.parse(responseText)).toEqual({
      ok: true,
      migrated: 1,
      sourceFinalized: true,
    })
    expect(responseText).not.toContain(secret)
    expect(uploads).toHaveLength(1)
    expect(uploads[0]?.label).toBe("Shared account")

    const source = await fetch(`${worker.baseURL}/tenant/accounts`, {
      headers: { authorization: `Bearer ${tenant.key}` },
    })
    expect(source.status).toBe(200)
    const sourceBody = (await source.json()) as { accounts: unknown[] }
    expect(sourceBody).toEqual({ accounts: [] })
  }, 60_000)

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
