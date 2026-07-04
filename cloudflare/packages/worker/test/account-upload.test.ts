import { afterEach, describe, expect, test } from "bun:test"
import { mkdtemp } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"

const adminToken = "admin-test-token"

const processes: Bun.Subprocess[] = []
const servers: Array<{ stop: () => void }> = []

afterEach(async () => {
  for (const proc of processes.splice(0)) {
    proc.kill()
    await proc.exited.catch(() => {})
  }
  for (const server of servers.splice(0)) server.stop()
})

describe("tenant account upload API", () => {
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

    const baseURL = await startWorker(upstream.url.origin)
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

const startWorker = async (upstreamOrigin: string): Promise<string> => {
  const workerPort = 20_000 + Math.floor(Math.random() * 1_000)
  const persistDir = await mkdtemp(join(tmpdir(), "subrouter-upload-test-"))
  const worker = Bun.spawn(
    [
      "bunx",
      "wrangler",
      "dev",
      "--local",
      "--ip",
      "127.0.0.1",
      "--port",
      String(workerPort),
      "--persist-to",
      persistDir,
      "--var",
      `ADMIN_TOKEN:${adminToken}`,
      "--var",
      `CLAUDE_UPSTREAM:${upstreamOrigin}`,
      "--log-level",
      "error",
    ],
    {
      cwd: import.meta.dir.replace(/\/test$/, ""),
      stdout: "pipe",
      stderr: "pipe",
    }
  )
  processes.push(worker)
  const baseURL = `http://127.0.0.1:${workerPort}`
  await waitForWorker(baseURL)
  return baseURL
}

const createTenant = async (
  baseURL: string,
  name: string
): Promise<{ readonly id: string; readonly key: string }> => {
  const response = await fetch(`${baseURL}/admin/tenants`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${adminToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ name }),
  })
  expect(response.status).toBe(200)
  return (await response.json()) as { id: string; key: string }
}

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

const waitForCondition = async (
  condition: () => boolean,
  label: string
): Promise<void> => {
  const deadline = Date.now() + 12_000
  while (Date.now() < deadline) {
    if (condition()) return
    await Bun.sleep(100)
  }
  throw new Error(`${label} timed out`)
}

const waitForWorker = async (baseURL: string): Promise<void> => {
  const deadline = Date.now() + 20_000
  let lastError: unknown
  while (Date.now() < deadline) {
    try {
      const response = await fetch(`${baseURL}/healthz`)
      if (response.ok) return
      lastError = new Error(`healthz returned ${response.status}`)
    } catch (error) {
      lastError = error
    }
    await Bun.sleep(250)
  }
  throw lastError ?? new Error("worker did not start")
}
