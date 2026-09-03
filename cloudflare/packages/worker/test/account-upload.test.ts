import { afterEach, describe, expect, test } from "bun:test"
import {
  cleanupWorkers,
  createTenant,
  startWorker,
  waitForCondition,
} from "./helpers/worker.ts"

const servers: Array<{ stop: () => void }> = []

afterEach(async () => {
  await cleanupWorkers()
  for (const server of servers.splice(0)) server.stop()
})

describe("tenant account upload API", () => {
  test("surfaces quota health and clears it after a successful proxy response", async () => {
    let mode: "quota" | "ok" = "quota"
    const providerMessage = "You're out of extra usage. Add more at claude.ai/settings/usage"
    const upstream = Bun.serve({
      port: 0,
      async fetch() {
        if (mode === "quota") {
          return Response.json(
            {
              type: "error",
              error: {
                type: "rate_limit_error",
                message: providerMessage,
              },
            },
            { status: 429 }
          )
        }
        return Response.json({ ok: true })
      },
    })
    servers.push(upstream)

    const worker = await startWorker({ claudeUpstream: upstream.url.origin })
    const tenant = await createTenant(worker.baseURL, "Quota Health Tenant")
    const account = await uploadAccount(worker.baseURL, tenant.key, {
      provider: "anthropic-apikey",
      label: "shared claude",
      apiKey: "sk-ant-quota-health-secret",
    }, ["sk-ant-quota-health-secret"])
    expect(account.health).toEqual({ ok: true })

    const quota = await fetch(`${worker.baseURL}/v1/messages`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenant.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Account-ID": account.id,
        "X-Subrouter-Session": "quota-health-session",
      },
      body: JSON.stringify({ model: "claude-3-5-sonnet-latest", messages: [] }),
    })
    expect(quota.status).toBe(429)
    const quotaBody = (await quota.json()) as Record<string, any>
    expect(quotaBody.error.message).toContain("shared claude")
    expect(quotaBody.error.message).toContain("cmux-subrouter status")
    expect(quotaBody.error.message).toContain(providerMessage)

    const degradedAccounts = await waitForTenantAccounts(
      worker.baseURL,
      tenant.key,
      (accounts) => accounts[0]?.health?.ok === false
    )
    expect(degradedAccounts[0]?.health).toMatchObject({
      ok: false,
      message: "Upstream reported quota exhaustion for this shared tenant account.",
    })
    expect(degradedAccounts[0]?.health?.lastQuotaErrorAt).toEqual(expect.any(String))
    const degradedText = JSON.stringify(degradedAccounts)
    expect(degradedText).not.toContain("sk-ant-quota-health-secret")

    const ready = await fetch(`${worker.baseURL}/_subrouter/ready?tenant=${tenant.id}`)
    expect(ready.status).toBe(200)
    const readyBody = (await ready.json()) as Record<string, unknown>
    expect(readyBody).toEqual({
      ok: true,
      draining: false,
      accountsDegraded: 1,
    })

    mode = "ok"
    const ok = await fetch(`${worker.baseURL}/v1/messages`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenant.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Account-ID": account.id,
        "X-Subrouter-Session": "quota-health-clear-session",
      },
      body: JSON.stringify({ model: "claude-3-5-sonnet-latest", messages: [] }),
    })
    expect(ok.status).toBe(200)
    const okBody = (await ok.json()) as Record<string, unknown>
    expect(okBody).toEqual({ ok: true })

    const healthyAccounts = await waitForTenantAccounts(
      worker.baseURL,
      tenant.key,
      (accounts) => accounts[0]?.health?.ok === true
    )
    expect(healthyAccounts[0]?.health).toEqual({ ok: true })

    const readyAfterClear = await fetch(`${worker.baseURL}/_subrouter/ready?tenant=${tenant.id}`)
    const readyAfterClearBody = (await readyAfterClear.json()) as Record<string, unknown>
    expect(readyAfterClearBody).toEqual({
      ok: true,
      draining: false,
      accountsDegraded: 0,
    })
  }, 60_000)

  test("times out validation fetches without storing the account", async () => {
    const hangingUsage = Bun.serve({
      port: 0,
      fetch() {
        return new Promise<Response>(() => {})
      },
    })
    servers.push(hangingUsage)

    const worker = await startWorker({ vars: { VALIDATION_TIMEOUT_MS: 50 } })
    const tenant = await createTenant(worker.baseURL, "Validation Timeout Tenant")
    const response = await fetch(`${worker.baseURL}/tenant/accounts?validate=1`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenant.key}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        provider: "claude",
        label: "slow-validation@example.com",
        claudeAiOauth: {
          accessToken: "slow-validation-access",
          refreshToken: "slow-validation-refresh",
          expiresAt: Date.now() + 3_600_000,
          subscriptionType: "max",
          usageUrl: `${hangingUsage.url.origin}/usage`,
        },
      }),
    })
    expect(response.status).toBe(400)
    expect(await response.text()).toContain("Account validation failed")

    const list = await fetch(`${worker.baseURL}/tenant/accounts`, {
      headers: { Authorization: `Bearer ${tenant.key}` },
    })
    expect(list.status).toBe(200)
    const body = (await list.json()) as { accounts: unknown[] }
    expect(body.accounts).toEqual([])
  }, 60_000)

  test("accepts a long-lived Claude setup token without a refresh token and rejects an expired one", async () => {
    const worker = await startWorker()
    const tenant = await createTenant(worker.baseURL, "Setup Token Tenant")
    const setupToken = "sk-ant-oat01-setup-token-secret-value-0123456789abcdef"
    const account = await uploadAccount(worker.baseURL, tenant.key, {
      provider: "claude",
      label: "setup@example.com",
      claudeAiOauth: {
        accessToken: setupToken,
        expiresAt: Date.now() + 365 * 24 * 60 * 60 * 1000,
        subscriptionType: "max",
        scopes: ["user:inference"],
      },
    }, [setupToken])
    expect(account.provider).toBe("claude")
    expect(account.auth_mode).toBe("oauth")

    const expired = await fetch(`${worker.baseURL}/tenant/accounts`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenant.key}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        provider: "claude",
        label: "stale@example.com",
        claudeAiOauth: {
          accessToken: setupToken,
          expiresAt: Date.now() - 1_000,
        },
      }),
    })
    expect(expired.status).toBe(400)
    expect(await expired.text()).toContain("Expired long-lived")
  }, 60_000)

  test("round-trips providers, sanitizes secrets, deletes accounts, and refreshes uploaded OAuth credentials", async () => {
    const refreshTokens: string[] = []
    const authServer = Bun.serve({
      port: 0,
      async fetch(request) {
        if (new URL(request.url).pathname !== "/oauth/token") {
          return new Response("Not Found", { status: 404 })
        }
        const body = await request.text()
        const params = new URLSearchParams(body)
        const refreshToken = params.get("refresh_token") ?? ""
        refreshTokens.push(refreshToken)
        if (refreshToken === "old-refresh-token") {
          return Response.json({
            access_token: "rotated-access-token-1",
            refresh_token: "rotated-refresh-token-1",
            expires_in: 1,
          })
        }
        if (refreshToken === "rotated-refresh-token-1") {
          return Response.json({
            access_token: "rotated-access-token-2",
            refresh_token: "rotated-refresh-token-2",
            expires_in: 3600,
          })
        }
        if (refreshToken === "delete-refresh-token") {
          return Response.json({
            access_token: "deleted-account-access-token",
            refresh_token: "deleted-account-refresh-token",
            expires_in: 3600,
          })
        }
        return Response.json({ error: "unexpected refresh token" }, { status: 400 })
      },
    })
    servers.push(authServer)

    let upstreamAuth: string | null = null
    const upstream = Bun.serve({
      port: 0,
      async fetch(request) {
        upstreamAuth = request.headers.get("Authorization")
        return Response.json({ ok: true })
      },
    })
    servers.push(upstream)

    const worker = await startWorker({ claudeUpstream: upstream.url.origin })
    const baseURL = worker.baseURL
    const tenant = await createTenant(baseURL, "Upload Tenant")

    const secrets = [
      "claude-access-token",
      "old-refresh-token",
      "sk-ant-secret-api-key",
      "codex-access-secret",
      "codex-refresh-secret",
      "codex-id-secret",
      "sk-openai-secret-key",
    ]

    const claude = await uploadAccount(baseURL, tenant.key, {
      provider: "claude",
      label: "claude@example.com",
      claudeAiOauth: {
        accessToken: "claude-access-token",
        refreshToken: "old-refresh-token",
        expiresAt: Date.now() + 2_500,
        subscriptionType: "max",
        rateLimitTier: "tier-1",
        tokenEndpoint: `${authServer.url.origin}/oauth/token`,
      },
    }, secrets)
    expect(claude.provider).toBe("claude")
    expect(claude.auth_mode).toBe("oauth")
    expect(claude.email).toBe("claude@example.com")

    const anthropic = await uploadAccount(baseURL, tenant.key, {
      provider: "anthropic-apikey",
      label: "anthropic key",
      apiKey: "sk-ant-secret-api-key",
    }, secrets)
    expect(anthropic.provider).toBe("claude")
    expect(anthropic.auth_mode).toBe("apikey")

    const codex = await uploadAccount(baseURL, tenant.key, {
      provider: "codex",
      label: "codex@example.com",
      tokens: {
        accessToken: "codex-access-secret",
        refreshToken: "codex-refresh-secret",
        idToken: "codex-id-secret",
        accountID: "chatgpt-account-id",
      },
    }, secrets)
    expect(codex.provider).toBe("codex")
    expect(codex.auth_mode).toBe("oauth")

    const openai = await uploadAccount(baseURL, tenant.key, {
      provider: "openai-apikey",
      label: "openai key",
      apiKey: "sk-openai-secret-key",
    }, secrets)
    expect(openai.provider).toBe("codex")
    expect(openai.auth_mode).toBe("apikey")

    const list = await fetch(`${baseURL}/tenant/accounts`, {
      headers: { Authorization: `Bearer ${tenant.key}` },
    })
    expect(list.status).toBe(200)
    const listText = await list.text()
    expect(JSON.parse(listText).accounts).toHaveLength(4)
    for (const secret of secrets) expect(listText).not.toContain(secret)

    const badShape = await fetch(`${baseURL}/tenant/accounts`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenant.key}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ provider: "openai-apikey" }),
    })
    expect(badShape.status).toBe(400)
    expect(await badShape.text()).toContain("Missing apiKey")

    await waitForCondition(
      () => refreshTokens.length >= 1,
      "uploaded OAuth alarm refresh"
    )
    expect(refreshTokens[0]).toBe("old-refresh-token")
    await Bun.sleep(1_300)

    const proxied = await fetch(`${baseURL}/v1/messages`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenant.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Account-ID": claude.id,
        "X-Subrouter-Session": "upload-refresh-session",
      },
      body: JSON.stringify({ model: "claude-3-haiku", messages: [] }),
    })
    expect(proxied.status).toBe(200)
    expect(refreshTokens.slice(0, 2)).toEqual([
      "old-refresh-token",
      "rotated-refresh-token-1",
    ])
    expect(upstreamAuth as string | null).toBe("Bearer rotated-access-token-2")

    const deleteMe = await uploadAccount(baseURL, tenant.key, {
      provider: "claude",
      label: "delete-me@example.com",
      claudeAiOauth: {
        accessToken: "delete-access-token",
        refreshToken: "delete-refresh-token",
        expiresAt: Date.now() + 2_500,
        subscriptionType: "max",
        tokenEndpoint: `${authServer.url.origin}/oauth/token`,
      },
    }, ["delete-access-token", "delete-refresh-token"])
    const beforeDeleteWait = refreshTokens.length
    const deleted = await fetch(`${baseURL}/tenant/accounts/${deleteMe.id}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${tenant.key}` },
    })
    expect(deleted.status).toBe(200)
    await Bun.sleep(3_000)
    expect(refreshTokens).toHaveLength(beforeDeleteWait)
  }, 90_000)
})

const uploadAccount = async (
  baseURL: string,
  tenantKey: string,
  body: Record<string, unknown>,
  secrets: ReadonlyArray<string>
): Promise<Record<string, any>> => {
  const response = await fetch(`${baseURL}/tenant/accounts`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${tenantKey}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
  })
  expect(response.status).toBe(200)
  const text = await response.text()
  for (const secret of secrets) expect(text).not.toContain(secret)
  return JSON.parse(text) as Record<string, any>
}

const waitForTenantAccounts = async (
  baseURL: string,
  tenantKey: string,
  predicate: (accounts: Array<Record<string, any>>) => boolean
): Promise<Array<Record<string, any>>> => {
  const deadline = Date.now() + 10_000
  let lastAccounts: Array<Record<string, any>> = []
  while (Date.now() < deadline) {
    const response = await fetch(`${baseURL}/tenant/accounts`, {
      headers: { Authorization: `Bearer ${tenantKey}` },
    })
    expect(response.status).toBe(200)
    const body = (await response.json()) as { accounts: Array<Record<string, any>> }
    lastAccounts = body.accounts
    if (predicate(lastAccounts)) return lastAccounts
    await Bun.sleep(100)
  }
  throw new Error(`tenant accounts did not reach expected health: ${JSON.stringify(lastAccounts)}`)
}
