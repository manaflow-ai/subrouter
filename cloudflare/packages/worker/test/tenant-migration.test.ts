import { afterEach, describe, expect, test } from "bun:test"
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
  test("moves credentials directly to an allowlisted hosted tenant without returning them", async () => {
    const uploads: Array<Record<string, any>> = []
    const destination = Bun.serve({
      port: 0,
      async fetch(request) {
        expect(new URL(request.url).pathname).toBe("/_subrouter/accounts")
        expect(request.headers.get("authorization")).toBe(
          "Bearer srt_0123456789abcdef0123456789abcdef"
        )
        uploads.push(await request.json() as Record<string, any>)
        return Response.json({
          account: {
            id: uploads.at(-1)?.accountId,
            kind: "codex",
            label: uploads.at(-1)?.label,
          },
        })
      },
    })
    servers.push(destination)

    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Legacy team")
    const secrets = [
      "legacy-access-a",
      "legacy-refresh-a",
      "legacy-id-a",
      "legacy-access-b",
      "legacy-refresh-b",
      "legacy-id-b",
    ]
    for (const suffix of ["a", "b"]) {
      const response = await fetch(`${worker.baseURL}/tenant/accounts`, {
        method: "POST",
        headers: {
          authorization: `Bearer ${tenant.key}`,
          "content-type": "application/json",
        },
        body: JSON.stringify({
          provider: "codex",
          label: "Shared account",
          tokens: {
            accessToken: `legacy-access-${suffix}`,
            refreshToken: `legacy-refresh-${suffix}`,
            idToken: `legacy-id-${suffix}`,
            accountID: `provider-${suffix}`,
          },
        }),
      })
      expect(response.status).toBe(200)
    }

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
        }),
      }
    )
    expect(response.status).toBe(200)
    const responseText = await response.text()
    expect(JSON.parse(responseText)).toEqual({ ok: true, migrated: 2 })
    for (const secret of [...secrets, tenant.key]) {
      expect(responseText).not.toContain(secret)
    }

    expect(uploads).toHaveLength(2)
    expect(new Set(uploads.map((upload) => upload.accountId)).size).toBe(2)
    expect(uploads.map((upload) => upload.label)).toEqual([
      "Shared account",
      "Shared account",
    ])
    expect(uploads.map((upload) => upload.tokens?.refreshToken).sort()).toEqual([
      "legacy-refresh-a",
      "legacy-refresh-b",
    ])
  }, 60_000)
})
