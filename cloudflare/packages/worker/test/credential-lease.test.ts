import { afterEach, describe, expect, test } from "bun:test"
import {
  cleanupWorkers,
  createTenant,
  startWorker,
} from "./helpers/worker.ts"

const servers: Array<{ stop: () => void }> = []

afterEach(async () => {
  await cleanupWorkers()
  for (const server of servers.splice(0)) server.stop()
})

describe("tenant credential leases", () => {
  test("refreshes centrally once, returns access-only leases, and persists rotation", async () => {
    const refreshTokens: string[] = []
    let refreshCount = 0
    const tokenServer = Bun.serve({
      port: 0,
      async fetch(request) {
        const form = new URLSearchParams(await request.text())
        refreshTokens.push(form.get("refresh_token") ?? "")
        refreshCount++
        return Response.json({
          access_token: `claude-access-${refreshCount}`,
          refresh_token: `claude-refresh-${refreshCount}`,
          expires_in: 3600,
        })
      },
    })
    servers.push(tokenServer)

    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Lease Tenant")
    const uploaded = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "claude",
        label: "shared@example.com",
        claudeAiOauth: {
          accessToken: "claude-access-expired",
          refreshToken: "claude-refresh-original",
          expiresAt: Date.now() - 60_000,
          tokenEndpoint: `${tokenServer.url.origin}/token`,
        },
      }),
    })
    expect(uploaded.status).toBe(200)

    const [first, second] = await Promise.all([
      requestLease(worker.baseURL, tenant.key, "session-a"),
      requestLease(worker.baseURL, tenant.key, "session-b"),
    ])
    expect(first.response.status).toBe(200)
    expect(second.response.status).toBe(200)
    expect(refreshCount).toBe(1)
    expect(first.body.token).toBe("claude-access-1")
    expect(second.body.token).toBe("claude-access-1")
    expect(first.body.authMode).toBe("oauth")
    expect(first.body.provider).toBe("claude")
    expect(first.body.credentialGeneration).toBe(1)
    expect(Date.parse(first.body.expiresAt) - Date.parse(first.body.issuedAt)).toBe(5 * 60 * 1000)

    const serialized = JSON.stringify([first.body, second.body])
    expect(serialized).not.toContain("claude-refresh")
    expect(serialized).not.toContain("idToken")
    expect(serialized).not.toContain("refreshToken")

    const unauthorized = await fetch(
      `${worker.baseURL}/tenant/leases/${encodeURIComponent(first.body.leaseId)}/events`,
      {
        method: "POST",
        headers: tenantHeaders(tenant.key),
        body: JSON.stringify({ outcome: "unauthorized", statusCode: 401 }),
      },
    )
    expect(unauthorized.status).toBe(200)
    const unauthorizedBody = await unauthorized.json() as {
      ok: boolean
      refreshState: string
    }
    expect(unauthorizedBody).toEqual({ ok: true, refreshState: "refreshed" })
    expect(refreshTokens).toEqual([
      "claude-refresh-original",
      "claude-refresh-1",
    ])

    const staleUnauthorized = await fetch(
      `${worker.baseURL}/tenant/leases/${encodeURIComponent(second.body.leaseId)}/events`,
      {
        method: "POST",
        headers: tenantHeaders(tenant.key),
        body: JSON.stringify({ outcome: "unauthorized", statusCode: 401 }),
      },
    )
    expect(staleUnauthorized.status).toBe(200)
    const staleUnauthorizedBody = await staleUnauthorized.json() as {
      ok: boolean
    }
    expect(staleUnauthorizedBody).toEqual({ ok: true })
    expect(refreshCount).toBe(2)
  }, 60_000)

  test("adopts an OAuth refresh chain before acknowledging upload", async () => {
    let refreshCount = 0
    const tokenServer = Bun.serve({
      port: 0,
      fetch() {
        refreshCount++
        return Response.json({
          access_token: "central-access",
          refresh_token: "central-refresh",
          expires_in: 3600,
        })
      },
    })
    servers.push(tokenServer)

    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Adoption")
    const uploaded = await fetch(
      `${worker.baseURL}/tenant/accounts?adopt=1`,
      {
        method: "POST",
        headers: tenantHeaders(tenant.key),
        body: JSON.stringify({
          provider: "claude",
          label: "adopt@example.com",
          claudeAiOauth: {
            accessToken: "laptop-access",
            refreshToken: "laptop-refresh",
            expiresAt: Date.now() + 60 * 60 * 1000,
            tokenEndpoint: `${tokenServer.url.origin}/token`,
          },
        }),
      },
    )
    expect(uploaded.status).toBe(200)
    expect(refreshCount).toBe(1)
    const uploadedText = await uploaded.text()
    expect(uploadedText).not.toContain("central-access")
    expect(uploadedText).not.toContain("central-refresh")

    const lease = await requestLease(worker.baseURL, tenant.key, "adopted")
    expect(lease.response.status).toBe(200)
    expect(lease.body.token).toBe("central-access")
    expect(lease.body.credentialGeneration).toBe(1)
    expect(refreshCount).toBe(1)
  }, 60_000)

  test("keeps a failed OAuth adoption out of credential routing", async () => {
    const tokenServer = Bun.serve({
      port: 0,
      fetch() {
        return new Response("temporary provider failure", { status: 500 })
      },
    })
    servers.push(tokenServer)

    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Failed adoption")
    const uploaded = await fetch(
      `${worker.baseURL}/tenant/accounts?adopt=1`,
      {
        method: "POST",
        headers: tenantHeaders(tenant.key),
        body: JSON.stringify({
          provider: "claude",
          label: "must stay disabled",
          claudeAiOauth: {
            accessToken: "unadopted-laptop-access",
            refreshToken: "unadopted-laptop-refresh",
            expiresAt: Date.now() + 60 * 60 * 1000,
            tokenEndpoint: `${tokenServer.url.origin}/token`,
          },
        }),
      },
    )
    expect(uploaded.status).toBe(400)

    const lease = await requestLease(
      worker.baseURL,
      tenant.key,
      "failed-adoption",
    )
    expect(lease.response.status).toBe(409)
    expect(JSON.stringify(lease.body)).not.toContain("unadopted-laptop-access")
  }, 60_000)

  test("honors an OAuth-only lease requirement over a preferred API key", async () => {
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "OAuth-only lease")
    const apiKeyResponse = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "openai-apikey",
        label: "API key",
        apiKey: "sk-api-key-must-not-reach-chatgpt",
      }),
    })
    const apiKey = await apiKeyResponse.json() as { id: string }
    expect(apiKeyResponse.status).toBe(200)
    const oauthResponse = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "codex",
        label: "OAuth",
        tokens: {
          accessToken: "oauth-access",
          refreshToken: "oauth-refresh",
          idToken: "oauth-id",
          expiresAt: Date.now() + 60 * 60 * 1000,
        },
      }),
    })
    expect(oauthResponse.status).toBe(200)

    const lease = await fetch(`${worker.baseURL}/tenant/leases`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "codex",
        requiredAuthMode: "oauth",
        agentType: "codex",
        sessionId: "chatgpt-backend",
        preferAccountId: apiKey.id,
      }),
    })
    const body = await lease.json() as { authMode?: string; token?: string }
    expect(lease.status).toBe(200)
    expect(body.authMode).toBe("oauth")
    expect(body.token).toBe("oauth-access")
  }, 60_000)

  test("rejects lease events from another tenant", async () => {
    const worker = await startWorker()
    const owner = await createTenant(worker.baseURL, "Owner")
    const attacker = await createTenant(worker.baseURL, "Attacker")
    const uploaded = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(owner.key),
      body: JSON.stringify({
        provider: "anthropic-apikey",
        label: "shared key",
        apiKey: "sk-ant-lease-test",
      }),
    })
    expect(uploaded.status).toBe(200)

    const lease = await requestLease(worker.baseURL, owner.key, "session-owner")
    expect(lease.response.status).toBe(200)
    const crossTenant = await fetch(
      `${worker.baseURL}/tenant/leases/${encodeURIComponent(lease.body.leaseId)}/events`,
      {
        method: "POST",
        headers: tenantHeaders(attacker.key),
        body: JSON.stringify({ outcome: "success", statusCode: 200 }),
      },
    )
    expect(crossTenant.status).toBe(404)
    expect(await crossTenant.text()).not.toContain("sk-ant-lease-test")
  }, 60_000)

  test("removes a rejected API key from routing until it is repaired", async () => {
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Rejected API key")
    const first = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "anthropic-apikey",
        label: "rejected",
        apiKey: "sk-ant-rejected",
      }),
    })
    const firstBody = await first.json() as { id: string }
    expect(first.status).toBe(200)
    const second = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "anthropic-apikey",
        label: "healthy",
        apiKey: "sk-ant-healthy",
      }),
    })
    expect(second.status).toBe(200)

    const rejectedLease = await fetch(`${worker.baseURL}/tenant/leases`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "claude",
        agentType: "claude",
        sessionId: "rejected-key",
        preferAccountId: firstBody.id,
      }),
    })
    const rejectedLeaseBody = await rejectedLease.json() as {
      leaseId: string
      token: string
    }
    expect(rejectedLease.status).toBe(200)
    expect(rejectedLeaseBody.token).toBe("sk-ant-rejected")

    const unauthorized = await fetch(
      `${worker.baseURL}/tenant/leases/${encodeURIComponent(rejectedLeaseBody.leaseId)}/events`,
      {
        method: "POST",
        headers: tenantHeaders(tenant.key),
        body: JSON.stringify({ outcome: "unauthorized", statusCode: 401 }),
      },
    )
    expect(unauthorized.status).toBe(200)

    const accounts = await fetch(`${worker.baseURL}/tenant/accounts`, {
      headers: tenantHeaders(tenant.key),
    })
    const accountsBody = await accounts.json() as {
      accounts: Array<{ id: string; health: { ok: boolean; message?: string } }>
    }
    expect(
      accountsBody.accounts.find((account) => account.id === firstBody.id)?.health,
    ).toEqual({
      ok: false,
      message: "Credential requires repair before it can be leased.",
    })

    const replacement = await fetch(`${worker.baseURL}/tenant/leases`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "claude",
        agentType: "claude",
        sessionId: "healthy-key",
        preferAccountId: firstBody.id,
      }),
    })
    const replacementBody = await replacement.json() as { token: string }
    expect(replacement.status).toBe(200)
    expect(replacementBody.token).toBe("sk-ant-healthy")
  }, 60_000)

  test("routes around an account whose central refresh needs repair", async () => {
    let refreshCount = 0
    const tokenServer = Bun.serve({
      port: 0,
      fetch() {
        refreshCount++
        return Response.json(
          { error: "invalid_grant", error_description: "repair required" },
          { status: 400 },
        )
      },
    })
    servers.push(tokenServer)

    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Refresh fallback")
    const broken = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "claude",
        label: "broken oauth",
        claudeAiOauth: {
          accessToken: "expired-access",
          refreshToken: "broken-refresh",
          expiresAt: Date.now() - 60_000,
          tokenEndpoint: `${tokenServer.url.origin}/token`,
        },
      }),
    })
    const brokenBody = await broken.json() as { id: string }
    expect(broken.status).toBe(200)
    const healthy = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "anthropic-apikey",
        label: "healthy fallback",
        apiKey: "sk-ant-healthy-fallback",
      }),
    })
    expect(healthy.status).toBe(200)

    const lease = await fetch(`${worker.baseURL}/tenant/leases`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "claude",
        agentType: "claude",
        sessionId: "refresh-fallback",
        preferAccountId: brokenBody.id,
      }),
    })
    const leaseBody = await lease.json() as {
      authMode: string
      token: string
    }
    expect(lease.status).toBe(200)
    expect(leaseBody.authMode).toBe("apikey")
    expect(leaseBody.token).toBe("sk-ant-healthy-fallback")
    expect(refreshCount).toBe(1)

    const second = await fetch(`${worker.baseURL}/tenant/leases`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "claude",
        agentType: "claude",
        sessionId: "refresh-fallback-2",
        preferAccountId: brokenBody.id,
      }),
    })
    expect(second.status).toBe(200)
    expect(refreshCount).toBe(1)
  }, 60_000)

  test("repairs an OAuth account in place and leases only the replacement access token", async () => {
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Repair")
    const future = Date.now() + 60 * 60 * 1000
    const created = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "claude",
        label: "Repair me",
        claudeAiOauth: {
          accessToken: "old-access",
          refreshToken: "old-refresh",
          expiresAt: future,
        },
      }),
    })
    const createdBody = await created.json() as { id: string }
    expect(created.status).toBe(200)
    const oldLease = await requestLease(
      worker.baseURL,
      tenant.key,
      "pre-repair-session",
    )
    expect(oldLease.response.status).toBe(200)
    expect(oldLease.body.credentialGeneration).toBe(0)

    const repaired = await fetch(
      `${worker.baseURL}/tenant/accounts/${encodeURIComponent(createdBody.id)}/repair`,
      {
        method: "POST",
        headers: tenantHeaders(tenant.key),
        body: JSON.stringify({
          provider: "claude",
          label: "Repair me",
          claudeAiOauth: {
            accessToken: "replacement-access",
            refreshToken: "replacement-refresh",
            expiresAt: future,
          },
        }),
      },
    )
    expect(repaired.status).toBe(200)
    const repairedText = await repaired.text()
    expect(repairedText).not.toContain("replacement-access")
    expect(repairedText).not.toContain("replacement-refresh")

    const staleEvent = await fetch(
      `${worker.baseURL}/tenant/leases/${encodeURIComponent(oldLease.body.leaseId)}/events`,
      {
        method: "POST",
        headers: tenantHeaders(tenant.key),
        body: JSON.stringify({ outcome: "unauthorized", statusCode: 401 }),
      },
    )
    expect(staleEvent.status).toBe(404)

    const lease = await fetch(`${worker.baseURL}/tenant/leases`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "claude",
        agentType: "claude",
        sessionId: "repair-session",
        preferAccountId: createdBody.id,
      }),
    })
    const leaseBody = await lease.json() as {
      accountId: string
      token: string
      credentialGeneration: number
    }
    expect(lease.status).toBe(200)
    expect(leaseBody.accountId).toBe(createdBody.id)
    expect(leaseBody.token).toBe("replacement-access")
    expect(leaseBody.credentialGeneration).toBe(1)
  }, 60_000)

  test("does not let an in-flight old refresh overwrite a repaired credential", async () => {
    let signalOldRefreshStarted: (() => void) | undefined
    const oldRefreshStarted = new Promise<void>((resolve) => {
      signalOldRefreshStarted = resolve
    })
    let releaseOldRefresh: (() => void) | undefined
    const oldRefreshReleased = new Promise<void>((resolve) => {
      releaseOldRefresh = resolve
    })
    const oldTokenServer = Bun.serve({
      port: 0,
      async fetch() {
        signalOldRefreshStarted?.()
        await oldRefreshReleased
        return Response.json({
          access_token: "old-refresh-result",
          refresh_token: "old-refresh-chain",
          expires_in: 3600,
        })
      },
    })
    servers.push(oldTokenServer)

    let signalReplacementRefreshStarted: (() => void) | undefined
    const replacementRefreshStarted = new Promise<void>((resolve) => {
      signalReplacementRefreshStarted = resolve
    })
    const replacementTokenServer = Bun.serve({
      port: 0,
      fetch() {
        signalReplacementRefreshStarted?.()
        return Response.json({
          access_token: "replacement-refresh-result",
          refresh_token: "replacement-refresh-chain",
          expires_in: 3600,
        })
      },
    })
    servers.push(replacementTokenServer)

    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Concurrent repair")
    const created = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: tenantHeaders(tenant.key),
      body: JSON.stringify({
        provider: "claude",
        label: "Race",
        claudeAiOauth: {
          accessToken: "old-expired",
          refreshToken: "old-refresh",
          expiresAt: Date.now() - 60_000,
          tokenEndpoint: `${oldTokenServer.url.origin}/token`,
        },
      }),
    })
    const createdBody = await created.json() as { id: string }
    expect(created.status).toBe(200)

    const oldLeasePromise = requestLease(
      worker.baseURL,
      tenant.key,
      "old-refresh-in-flight",
    )
    await oldRefreshStarted

    const repairPromise = fetch(
      `${worker.baseURL}/tenant/accounts/${encodeURIComponent(createdBody.id)}/repair?adopt=1`,
      {
        method: "POST",
        headers: tenantHeaders(tenant.key),
        body: JSON.stringify({
          provider: "claude",
          label: "Race",
          claudeAiOauth: {
            accessToken: "replacement-before-adoption",
            refreshToken: "replacement-refresh",
            expiresAt: Date.now() + 60 * 60 * 1000,
            tokenEndpoint: `${replacementTokenServer.url.origin}/token`,
          },
        }),
      },
    )
    await Promise.race([
      replacementRefreshStarted,
      Bun.sleep(50),
    ])
    releaseOldRefresh?.()

    const [repaired, oldLease] = await Promise.all([
      repairPromise,
      oldLeasePromise,
    ])
    expect(repaired.status).toBe(200)
    expect(oldLease.response.status).toBe(200)

    const finalLease = await requestLease(
      worker.baseURL,
      tenant.key,
      "after-repair",
    )
    expect(finalLease.response.status).toBe(200)
    expect(finalLease.body.token).toBe("replacement-refresh-result")
  }, 60_000)
})

const requestLease = async (
  baseURL: string,
  tenantKey: string,
  sessionId: string,
): Promise<{
  response: Response
  body: {
    leaseId: string
    token: string
    provider: string
    authMode: string
    credentialGeneration: number
    issuedAt: string
    expiresAt: string
  }
}> => {
  const response = await fetch(`${baseURL}/tenant/leases`, {
    method: "POST",
    headers: tenantHeaders(tenantKey),
    body: JSON.stringify({
      provider: "claude",
      agentType: "claude",
      sessionId,
    }),
  })
  return {
    response,
    body: await response.json() as {
      leaseId: string
      token: string
      provider: string
      authMode: string
      credentialGeneration: number
      issuedAt: string
      expiresAt: string
    },
  }
}

const tenantHeaders = (tenantKey: string): HeadersInit => ({
  Authorization: `Bearer ${tenantKey}`,
  "Content-Type": "application/json",
})
