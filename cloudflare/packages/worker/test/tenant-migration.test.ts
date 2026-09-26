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
  stopWorker,
  waitForCondition,
} from "./helpers/worker.ts"

const servers: Array<{ stop: () => void }> = []

afterEach(async () => {
  await cleanupWorkers()
  for (const server of servers.splice(0)) server.stop()
})

describe("legacy tenant migration", () => {
  test("stages credentials without activating them before source finalization", async () => {
    const uploads: Array<Record<string, any>> = []
    const calls: string[] = []
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
        expect(String(input)).toBe(
          "https://sr.cmux.com/_subrouter/accounts/migration/stage"
        )
        expect(new Headers(init?.headers).get("authorization")).toBe(
          "Bearer srt_0123456789abcdef0123456789abcdef"
        )
        calls.push(new URL(String(input)).pathname)
        const body = JSON.parse(String(init?.body)) as Record<string, any>
        uploads.push(...body.accounts)
        return Response.json({
          ok: true,
          accountIds: body.accounts.map(
            (upload: Record<string, any>) => upload.accountId
          ),
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
    expect(calls).toEqual(["/_subrouter/accounts/migration/stage"])
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
      fetch: hostedMigrationFetch(),
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
        fetch: hostedMigrationFetch({ failStage: true }),
      })
    ).rejects.toThrow("destination is unavailable")
    expect(sourceState.value).toBe("active")
  })

  test("restores source routing when staging and inactive rollback both fail", async () => {
    const sourceState = {
      value: "active" as "active" | "quiesced" | "removed",
    }
    let recoveryPreserved = false
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
          restore: async (_accounts, options) => {
            sourceState.value = "active"
            recoveryPreserved = options.preserveRecovery
          },
        },
        fetch: hostedMigrationFetch({
          failStage: true,
          failRollback: true,
        }),
      })
    ).rejects.toThrow("destination is unavailable")
    expect(sourceState.value).toBe("active")
    expect(recoveryPreserved).toBe(true)
  })

  test("clears recovery after an authoritatively rejected stage", async () => {
    const sourceState = {
      value: "active" as "active" | "quiesced",
    }
    const destinationCalls: string[] = []
    let recoveryPreserved = true
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
          complete: async () => {},
          restore: async (_accounts, options) => {
            sourceState.value = "active"
            recoveryPreserved = options.preserveRecovery
          },
        },
        fetch: hostedMigrationFetch({
          rejectAuthorization: true,
          calls: destinationCalls,
        }),
      })
    ).rejects.toThrow("stage failed (401)")
    expect(sourceState.value).toBe("active")
    expect(recoveryPreserved).toBe(false)
    expect(destinationCalls).toEqual([
      "/_subrouter/accounts/migration/stage",
    ])
  })

  test("rejects unsafe migration identities before quiescing the source", async () => {
    const invalidAccounts = [
      { ...legacyAccount("control"), label: "unsafe\u001b[31m" },
      { ...legacyAccount("label"), label: "é".repeat(161) },
      { ...legacyAccount("id"), id: "é".repeat(161) },
    ]
    for (const account of invalidAccounts) {
      let sourceBegan = false
      const destinationCalls: string[] = []
      await expect(
        migrateLegacyTenant({
          destinationUrl: "https://sr.cmux.com",
          tenantKey: "srt_0123456789abcdef0123456789abcdef",
          finalizeSource: true,
          source: {
            list: async () => [account],
            begin: async () => {
              sourceBegan = true
              return [account]
            },
            complete: async () => {},
            restore: async () => {},
          },
          fetch: hostedMigrationFetch({ calls: destinationCalls }),
        })
      ).rejects.toThrow("legacy account cannot be migrated safely")
      expect(sourceBegan).toBe(false)
      expect(destinationCalls).toEqual([])
    }
  })

  test("accepts migration identities at the 320-byte UTF-8 boundary", async () => {
    const migrated = await migrateLegacyAccountsToHosted({
      destinationUrl: "https://sr.cmux.com",
      tenantKey: "srt_0123456789abcdef0123456789abcdef",
      accounts: [{
        ...legacyAccount("boundary"),
        id: "é".repeat(160),
        label: "é".repeat(160),
      }],
      fetch: hostedMigrationFetch(),
    })
    expect(migrated).toBe(1)
  })

  test("restores source routing when quiescing fails after changing state", async () => {
    const sourceState = {
      value: "active" as "active" | "quiesced",
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
            throw new Error("alarm update failed")
          },
          complete: async () => {},
          restore: async () => {
            sourceState.value = "active"
          },
        },
      })
    ).rejects.toThrow("alarm update failed")
    expect(sourceState.value).toBe("active")
  })

  test("finalizes source rows through the admin route after hosted upload", async () => {
    const uploads: Array<Record<string, any>> = []
    const destinationCalls: string[] = []
    const destination = Bun.serve({
      port: 0,
      async fetch(request) {
        destinationCalls.push(new URL(request.url).pathname)
        return await hostedMigrationResponse(request, { uploads })
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
    expect(destinationCalls).toEqual([
      "/_subrouter/accounts/migration/stage",
      "/_subrouter/accounts/migration/activate",
    ])

    const source = await fetch(`${worker.baseURL}/tenant/accounts`, {
      headers: { authorization: `Bearer ${tenant.key}` },
    })
    expect(source.status).toBe(200)
    const sourceBody = (await source.json()) as { accounts: unknown[] }
    expect(sourceBody).toEqual({ accounts: [] })
  }, 60_000)

  test("rejects finalization while preflight staging is in progress", async () => {
    let firstStageStarted = false
    let releaseFirstStage!: () => void
    const firstStageReleased = new Promise<void>((resolve) => {
      releaseFirstStage = resolve
    })
    let stageCount = 0
    const destination = Bun.serve({
      port: 0,
      async fetch(request) {
        if (new URL(request.url).pathname.endsWith("/stage")) {
          stageCount += 1
          if (stageCount === 1) {
            firstStageStarted = true
            await firstStageReleased
          }
        }
        return await hostedMigrationResponse(request)
      },
    })
    servers.push(destination)
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Serialized team")
    await uploadLegacyCodexAccount(
      worker.baseURL,
      tenant.key,
      "Serialized account",
      "serialized"
    )

    const preflight = migrateTenant(worker.baseURL, tenant.id, {
      destinationUrl: destination.url.origin,
      finalizeSource: false,
    })
    await waitForCondition(
      () => firstStageStarted,
      "hosted migration preflight to start"
    )
    const concurrentFinalization = await migrateTenant(
      worker.baseURL,
      tenant.id,
      {
        destinationUrl: destination.url.origin,
        finalizeSource: true,
      }
    )
    releaseFirstStage()
    const preflightResponse = await preflight

    expect(concurrentFinalization.status).toBe(409)
    expect(preflightResponse.status).toBe(200)
    expect(stageCount).toBe(1)

    const finalization = await migrateTenant(worker.baseURL, tenant.id, {
      destinationUrl: destination.url.origin,
      finalizeSource: true,
    })
    expect(finalization.status).toBe(200)
  }, 60_000)

  test("returns a completed receipt without touching Hosted on retry", async () => {
    const destinationCalls: string[] = []
    const destination = Bun.serve({
      port: 0,
      async fetch(request) {
        destinationCalls.push(new URL(request.url).pathname)
        return await hostedMigrationResponse(request)
      },
    })
    servers.push(destination)
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Completed team")
    await uploadLegacyCodexAccount(
      worker.baseURL,
      tenant.key,
      "Completed account",
      "completed"
    )

    const first = await migrateTenant(worker.baseURL, tenant.id, {
      destinationUrl: destination.url.origin,
      finalizeSource: true,
    })
    expect(first.status).toBe(200)
    const callsAfterSuccess = [...destinationCalls]

    const retry = await migrateTenant(worker.baseURL, tenant.id, {
      destinationUrl: destination.url.origin,
      finalizeSource: true,
    })
    expect(retry.status).toBe(200)
    const retryBody = (await retry.json()) as Record<string, unknown>
    expect(retryBody).toEqual({
      ok: true,
      migrated: 1,
      sourceFinalized: true,
    })
    expect(destinationCalls).toEqual(callsAfterSuccess)
  }, 60_000)

  test("rejects a concurrent account mutation after source finalization", async () => {
    let destinationStarted = false
    let releaseDestination!: () => void
    const destinationReleased = new Promise<void>((resolve) => {
      releaseDestination = resolve
    })
    const destination = Bun.serve({
      port: 0,
      async fetch(request) {
        if (new URL(request.url).pathname.endsWith("/stage")) {
          destinationStarted = true
          await destinationReleased
        }
        return await hostedMigrationResponse(request)
      },
    })
    servers.push(destination)
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Concurrent team")
    const created = await uploadLegacyCodexAccount(
      worker.baseURL,
      tenant.key,
      "Original account",
      "original"
    )

    const migration = migrateTenant(worker.baseURL, tenant.id, {
      destinationUrl: destination.url.origin,
      finalizeSource: true,
    })
    await waitForCondition(
      () => destinationStarted,
      "hosted migration upload to start"
    )
    const mutation = fetch(`${worker.baseURL}/admin/accounts`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${adminToken}`,
        "content-type": "application/json",
      },
      body: JSON.stringify({
        id: created.id,
        orgId: tenant.id,
        kind: "codex_oauth",
        label: "Concurrent account",
        enabled: true,
        credentials: codexCredentials("concurrent"),
      }),
    })
    releaseDestination()

    const [migrationResponse, mutationResponse] = await Promise.all([
      migration,
      mutation,
    ])
    expect(migrationResponse.status).toBe(200)
    expect(mutationResponse.status).toBe(400)
    const accounts = await listAdminAccounts(
      worker.baseURL,
      tenant.id
    )
    expect(accounts).toEqual([])
  }, 60_000)

  test("retains recovery binding after restart until matching rollback", async () => {
    let destinationStarted = false
    let hangStage = true
    const destinationCalls: string[] = []
    const destination = Bun.serve({
      port: 0,
      async fetch(request) {
        const path = new URL(request.url).pathname
        destinationCalls.push(path)
        if (path.endsWith("/stage") && hangStage) {
          destinationStarted = true
          return new Promise<Response>(() => {})
        }
        return await hostedMigrationResponse(request)
      },
    })
    servers.push(destination)
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Restarted team")
    await uploadLegacyCodexAccount(
      worker.baseURL,
      tenant.key,
      "Restarted account",
      "restart"
    )

    const migration = migrateTenant(worker.baseURL, tenant.id, {
      destinationUrl: destination.url.origin,
      finalizeSource: true,
    }).catch(() => null)
    await waitForCondition(
      () => destinationStarted,
      "hosted migration upload to start"
    )
    await stopWorker(worker)
    await migration

    const restarted = await startWorker({ persistDir: worker.persistDir })
    const accounts = await listAdminAccounts(restarted.baseURL, tenant.id)
    expect(accounts).toHaveLength(1)
    expect(accounts[0]).toMatchObject({
      label: "Restarted account",
      enabled: true,
      hasCredentials: true,
    })

    const wrongDestinationCalls: string[] = []
    const wrongDestination = Bun.serve({
      port: 0,
      async fetch(request) {
        wrongDestinationCalls.push(new URL(request.url).pathname)
        return await hostedMigrationResponse(request)
      },
    })
    servers.push(wrongDestination)
    const wrongRetry = await migrateTenant(restarted.baseURL, tenant.id, {
      destinationUrl: wrongDestination.url.origin,
      finalizeSource: true,
    })
    expect(wrongRetry.status).toBe(409)
    expect(wrongDestinationCalls).toEqual([])

    hangStage = false
    const callsBeforeRetry = destinationCalls.length
    const retry = await migrateTenant(restarted.baseURL, tenant.id, {
      destinationUrl: destination.url.origin,
      finalizeSource: true,
    })
    expect(retry.status).toBe(200)
    expect(destinationCalls.slice(callsBeforeRetry)).toEqual([
      "/_subrouter/accounts/migration/rollback",
      "/_subrouter/accounts/migration/stage",
      "/_subrouter/accounts/migration/activate",
    ])
  }, 60_000)

  test("keeps an ambiguous activation quiesced until rollback is confirmed", async () => {
    let activationStarted = false
    let hangActivation = true
    const destinationCalls: string[] = []
    const destination = Bun.serve({
      port: 0,
      async fetch(request) {
        const path = new URL(request.url).pathname
        destinationCalls.push(path)
        if (path.endsWith("/activate") && hangActivation) {
          activationStarted = true
          return new Promise<Response>(() => {})
        }
        return await hostedMigrationResponse(request)
      },
    })
    servers.push(destination)
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Ambiguous team")
    await uploadLegacyCodexAccount(
      worker.baseURL,
      tenant.key,
      "Ambiguous account",
      "ambiguous"
    )

    const migration = migrateTenant(worker.baseURL, tenant.id, {
      destinationUrl: destination.url.origin,
      finalizeSource: true,
    }).catch(() => null)
    await waitForCondition(
      () => activationStarted,
      "hosted migration activation to start"
    )
    await stopWorker(worker)
    await migration

    const restarted = await startWorker({ persistDir: worker.persistDir })
    const quiesced = await listAdminAccounts(restarted.baseURL, tenant.id)
    expect(quiesced).toHaveLength(1)
    expect(quiesced[0]?.enabled).toBe(false)

    hangActivation = false
    const retry = await migrateTenant(restarted.baseURL, tenant.id, {
      destinationUrl: destination.url.origin,
      finalizeSource: true,
    })
    expect(retry.status).toBe(200)
    expect(destinationCalls).toContain(
      "/_subrouter/accounts/migration/rollback"
    )
    expect(await listAdminAccounts(restarted.baseURL, tenant.id)).toEqual([])
  }, 60_000)

  test("rejects recovery against a different Hosted destination", async () => {
    let activationStarted = false
    const originalDestination = Bun.serve({
      port: 0,
      async fetch(request) {
        if (new URL(request.url).pathname.endsWith("/activate")) {
          activationStarted = true
          return new Promise<Response>(() => {})
        }
        return await hostedMigrationResponse(request)
      },
    })
    const wrongDestinationCalls: string[] = []
    const wrongDestination = Bun.serve({
      port: 0,
      async fetch(request) {
        wrongDestinationCalls.push(new URL(request.url).pathname)
        return await hostedMigrationResponse(request)
      },
    })
    servers.push(originalDestination, wrongDestination)
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Bound team")
    await uploadLegacyCodexAccount(
      worker.baseURL,
      tenant.key,
      "Bound account",
      "bound"
    )

    const migration = migrateTenant(worker.baseURL, tenant.id, {
      destinationUrl: originalDestination.url.origin,
      finalizeSource: true,
    }).catch(() => null)
    await waitForCondition(
      () => activationStarted,
      "bound migration activation to start"
    )
    await stopWorker(worker)
    await migration

    const restarted = await startWorker({ persistDir: worker.persistDir })
    const retry = await migrateTenant(restarted.baseURL, tenant.id, {
      destinationUrl: wrongDestination.url.origin,
      finalizeSource: true,
    })
    expect(retry.status).toBe(409)
    expect(wrongDestinationCalls).toEqual([])
    const accounts = await listAdminAccounts(restarted.baseURL, tenant.id)
    expect(accounts).toHaveLength(1)
    expect(accounts[0]?.enabled).toBe(false)
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

const codexCredentials = (suffix: string) => ({
  accessToken: `legacy-access-${suffix}`,
  refreshToken: `legacy-refresh-${suffix}`,
  idToken: `legacy-id-${suffix}`,
  accountId: `provider-${suffix}`,
})

const uploadLegacyCodexAccount = async (
  baseURL: string,
  tenantKey: string,
  label: string,
  suffix: string
): Promise<{ id: string }> => {
  const response = await fetch(`${baseURL}/tenant/accounts`, {
    method: "POST",
    headers: {
      authorization: `Bearer ${tenantKey}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({
      provider: "codex",
      label,
      tokens: {
        ...codexCredentials(suffix),
        accountID: `provider-${suffix}`,
      },
    }),
  })
  expect(response.status).toBe(200)
  return (await response.json()) as { id: string }
}

const migrateTenant = async (
  baseURL: string,
  tenantId: string,
  input: {
    readonly destinationUrl: string
    readonly finalizeSource: boolean
  }
): Promise<Response> =>
  await fetch(`${baseURL}/admin/tenants/${tenantId}/migrate-hosted`, {
    method: "POST",
    headers: {
      authorization: `Bearer ${adminToken}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({
      ...input,
      tenantKey: "srt_0123456789abcdef0123456789abcdef",
    }),
  })

const listAdminAccounts = async (
  baseURL: string,
  tenantId: string
): Promise<Array<Record<string, any>>> => {
  const response = await fetch(
    `${baseURL}/admin/accounts?tenant=${encodeURIComponent(tenantId)}`,
    { headers: { authorization: `Bearer ${adminToken}` } }
  )
  expect(response.status).toBe(200)
  const body = (await response.json()) as {
    accounts: Array<Record<string, any>>
  }
  return body.accounts
}

const hostedMigrationFetch = (
  options: {
    readonly failStage?: boolean
    readonly failRollback?: boolean
    readonly rejectAuthorization?: boolean
    readonly calls?: string[]
  } = {}
): typeof fetch =>
  Object.assign(
    async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
      const request = new Request(String(input), init)
      options.calls?.push(new URL(request.url).pathname)
      if (options.rejectAuthorization) {
        return new Response("unauthorized", { status: 401 })
      }
      if (
        options.failStage &&
        new URL(request.url).pathname.endsWith("/stage")
      ) {
        throw new Error("network body with secret")
      }
      if (
        options.failRollback &&
        new URL(request.url).pathname.endsWith("/rollback")
      ) {
        throw new Error("rollback is unavailable")
      }
      return await hostedMigrationResponse(request)
    },
    { preconnect() {} }
  ) as typeof fetch

const hostedMigrationResponse = async (
  request: Request,
  options: { readonly uploads?: Array<Record<string, any>> } = {}
): Promise<Response> => {
  const path = new URL(request.url).pathname
  const body = (await request.json()) as Record<string, any>
  if (path.endsWith("/stage")) {
    const accounts = body.accounts as Array<Record<string, any>>
    options.uploads?.push(...accounts)
    return Response.json({
      ok: true,
      accountIds: accounts.map((account) => account.accountId),
    })
  }
  if (path.endsWith("/activate")) {
    return Response.json({ ok: true, activated: body.accountIds })
  }
  if (path.endsWith("/rollback")) {
    return Response.json({ ok: true, rolledBack: true })
  }
  return new Response("not found", { status: 404 })
}
