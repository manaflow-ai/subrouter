import { afterEach, describe, expect, test } from "bun:test"
import { mkdtemp } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"

const adminToken = "admin-test-token"
const proxyToken = "proxy-test-token"

const processes: Bun.Subprocess[] = []
const servers: Array<{ stop: () => void }> = []

afterEach(async () => {
  for (const proc of processes.splice(0)) {
    proc.kill()
    await proc.exited.catch(() => {})
  }
  for (const server of servers.splice(0)) server.stop()
})

describe("subrouter Durable Object Worker transcripts", () => {
  test("refreshes near-expiry OAuth credentials from a Durable Object alarm", async () => {
    let refreshCalls = 0
    const authServer = Bun.serve({
      port: 0,
      async fetch(request) {
        if (new URL(request.url).pathname !== "/oauth/token") {
          return new Response("Not Found", { status: 404 })
        }
        refreshCalls += 1
        const body = await request.json() as Record<string, unknown>
        expect(body.grant_type).toBe("refresh_token")
        expect(body.refresh_token).toBe("old-refresh-token")
        return Response.json({
          access_token: "alarm-refreshed-access-token",
          refresh_token: "alarm-refreshed-refresh-token",
          id_token: "alarm-refreshed-id-token",
        })
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

    const baseURL = await startWorker(`${upstream.url.origin}/backend-api/codex`)
    const tenant = await createTenant(baseURL, "Refresh Org")
    await upsertAccount(baseURL, {
      id: "acct-refresh",
      orgId: tenant.id,
      kind: "codex_oauth",
      label: "refresh@example.com",
      credentials: {
        accessToken: "old-access-token",
        refreshToken: "old-refresh-token",
        idToken: "old-id-token",
        expiresAt: Date.now() + 2_500,
        tokenEndpoint: `${authServer.url.origin}/oauth/token`,
      },
    })

    await waitForCondition(() => refreshCalls >= 1, "Durable Object alarm refresh")
    expect(refreshCalls).toBe(1)

    const proxied = await fetch(`${baseURL}/v1/responses`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenant.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Session": "refresh-session",
      },
      body: "{}",
    })
    expect(proxied.status).toBe(200)
    expect(upstreamAuth as string | null).toBe("Bearer alarm-refreshed-access-token")
    expect(refreshCalls).toBe(1)
  }, 60_000)

  test("keeps admin state, routing, sessions, and lifecycle scoped per org", async () => {
    const baseURL = await startWorker("https://codex.invalid/backend-api/codex")
    const tenantA = await createTenant(baseURL, "Org A")
    const tenantB = await createTenant(baseURL, "Org B")
    await upsertAccount(baseURL, {
      id: "acct-a",
      orgId: tenantA.id,
      kind: "openai_apikey",
      label: "org-a@example.com",
      credentials: { apiKey: "sk-org-a" },
    })
    await upsertAccount(baseURL, {
      id: "acct-b",
      orgId: tenantB.id,
      kind: "openai_apikey",
      label: "org-b@example.com",
      credentials: { apiKey: "sk-org-b" },
    })

    const unauthorizedProxy = await fetch(`${baseURL}/v1/responses`, {
      method: "POST",
      body: "{}",
    })
    expect(unauthorizedProxy.status).toBe(401)

    const orgAAccounts = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/accounts?tenant=${tenantA.id}`
    )
    const orgBAccounts = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/accounts?tenant=${tenantB.id}`
    )
    expect(orgAAccounts.map((account) => account.id)).toEqual(["acct-a"])
    expect(orgBAccounts.map((account) => account.id)).toEqual(["acct-b"])
    expect(JSON.stringify(orgAAccounts)).not.toContain("sk-org-a")

    const orgAStatus = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/account-status?tenant=${tenantA.id}`
    )
    expect(orgAStatus.map((account) => account.id)).toEqual(["acct-a"])

    const orgAUsage = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/usage-status?tenant=${tenantA.id}`
    )
    expect(orgAUsage.map((account) => account.id)).toEqual(["acct-a"])
    expect(orgAUsage[0]?.plan_type).toBe("api key")

    const routeA = await fetch(`${baseURL}/route`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenantA.key}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ sessionId: "shared-session", model: "gpt-5" }),
    })
    expect(routeA.status).toBe(200)
    const routeABody = (await routeA.json()) as Record<string, any>
    expect(routeABody.account.id).toBe("acct-a")

    const routeB = await fetch(`${baseURL}/route`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenantB.key}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ sessionId: "shared-session", model: "gpt-5" }),
    })
    expect(routeB.status).toBe(200)
    const routeBBody = (await routeB.json()) as Record<string, any>
    expect(routeBBody.account.id).toBe("acct-b")

    const crossOrgUsage = await fetch(`${baseURL}/usage`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenantA.key}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ sessionId: "shared-session", accountId: "acct-b" }),
    })
    expect(crossOrgUsage.status).toBe(400)

    const sessionsA = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/sessions?tenant=${tenantA.id}`
    )
    const sessionsB = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/sessions?tenant=${tenantB.id}`
    )
    expect(sessionsA).toHaveLength(1)
    expect(sessionsA[0]?.account_id).toBe("acct-a")
    expect(sessionsB).toHaveLength(1)
    expect(sessionsB[0]?.account_id).toBe("acct-b")

    expect((await fetch(`${baseURL}/_subrouter/ready`)).status).toBe(200)
    const drainA = await fetch(`${baseURL}/_subrouter/drain?tenant=${tenantA.id}`, {
      method: "POST",
      headers: { Authorization: `Bearer ${adminToken}` },
    })
    expect(drainA.status).toBe(200)
    const drainStatusA = await adminJSON<Record<string, any>>(
      baseURL,
      `/_subrouter/drain-status?tenant=${tenantA.id}`
    )
    const drainStatusB = await adminJSON<Record<string, any>>(
      baseURL,
      `/_subrouter/drain-status?tenant=${tenantB.id}`
    )
    expect(drainStatusA.draining).toBe(true)
    expect(drainStatusB.draining).toBe(false)
  }, 60_000)

  test("records scoped HTTP transcripts with sanitized and raw endpoints", async () => {
    const upstreamRequests: Array<{ path: string; auth: string | null; body: string }> = []
    const upstream = Bun.serve({
      port: 0,
      async fetch(request) {
        const url = new URL(request.url)
        upstreamRequests.push({
          path: url.pathname,
          auth: request.headers.get("Authorization"),
          body: await request.text(),
        })
        return Response.json({
          response: {
            model: "gpt-5.5",
            usage: {
              input_tokens: 100,
              output_tokens: 7,
              total_tokens: 107,
            },
          },
        })
      },
    })
    servers.push(upstream)

    const baseURL = await startWorker(`${upstream.url.origin}/backend-api/codex`)
    const tenant = await createTenant(baseURL, "Transcript Org")
    await upsertCodexAccount(baseURL, tenant.id, "acct-a")

    const proxied = await fetch(`${baseURL}/v1/responses`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenant.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Session": "session-1:turn-0",
        "X-Subrouter-User-Email": "symphony@manaflow.ai",
      },
      body: JSON.stringify({ model: "gpt-5.5", input: "secret body" }),
    })
    expect(proxied.status).toBe(200)
    expect(upstreamRequests).toHaveLength(1)
    expect(upstreamRequests[0]?.path).toBe("/backend-api/codex/responses")
    expect(upstreamRequests[0]?.auth).toBe("Bearer codex-access-token")

    const list = await fetch(`${baseURL}/_subrouter/transcripts?tenant=${tenant.id}`, {
      headers: { Authorization: `Bearer ${adminToken}` },
    })
    expect(list.status).toBe(200)
    const summaries = (await list.json()) as Array<Record<string, any>>
    expect(summaries).toHaveLength(1)
    expect(summaries[0]?.agent_type).toBe("codex")
    expect(summaries[0]?.session_id).toBe("session-1")
    expect(summaries[0]?.event_count).toBe(3)
    expect(summaries[0]?.has_bodies).toBe(true)
    expect(summaries[0]?.usage.total_tokens).toBe(107)
    expect(summaries[0]?.user).toBe("symphony@manaflow.ai")
    expect(summaries[0]?.account).toBe("acct-a")

    const otherOrgList = await fetch(`${baseURL}/_subrouter/transcripts?tenant=org-b`, {
      headers: { Authorization: `Bearer ${adminToken}` },
    })
    expect((await otherOrgList.json()) as unknown[]).toEqual([])

    const detail = await fetch(
      `${baseURL}/_subrouter/transcripts/codex/session-1?tenant=${tenant.id}`,
      { headers: { Authorization: `Bearer ${adminToken}` } }
    )
    const detailText = await detail.text()
    expect(detail.status).toBe(200)
    expect(detailText).not.toContain("body_base64\"")
    expect(detailText).not.toContain(proxyToken)
    expect(detailText).not.toContain("codex-access-token")
    expect(detailText).toContain("body_base64_redacted")
    expect(detailText).toContain("<redacted>")

    const raw = await fetch(
      `${baseURL}/_subrouter/transcripts/codex/session-1/raw?tenant=${tenant.id}`,
      { headers: { Authorization: `Bearer ${adminToken}` } }
    )
    const rawText = await raw.text()
    expect(raw.status).toBe(200)
    expect(rawText).toContain("secret body")
    expect(rawText).toContain("gpt-5.5")

    const dashboard = await fetch(`${baseURL}/_subrouter/dashboard?tenant=${tenant.id}`, {
      headers: { Authorization: `Bearer ${adminToken}` },
    })
    const dashboardText = await dashboard.text()
    expect(dashboard.status).toBe(200)
    expect(dashboardText).toContain("session-1")
    expect(dashboardText).toContain("Total Tokens")
    expect(dashboardText).toContain("Model Calls")
    expect(dashboardText).toContain("Tokens Over Time")
    expect(dashboardText).toContain("Models")
    expect(dashboardText).toContain("Users")
    expect(dashboardText).toContain("Accounts")
    expect(dashboardText).toContain("107")
    expect(dashboardText).toContain("gpt-5.5")
    expect(dashboardText).toContain("symphony@manaflow.ai")
    expect(dashboardText).toContain("acct-a")
  }, 60_000)

  test("records proxied WebSocket messages in both directions", async () => {
    let upstreamAuth: string | null = null
    const upstream = Bun.serve({
      port: 0,
      fetch(request, server) {
        upstreamAuth = request.headers.get("Authorization")
        if (server.upgrade(request)) return undefined
        return new Response("Expected WebSocket", { status: 426 })
      },
      websocket: {
        message(socket, message) {
          socket.send(
            JSON.stringify({
              response: {
                model: "gpt-5.5",
                usage: {
                  input_tokens: 8,
                  output_tokens: 3,
                  total_tokens: 11,
                },
              },
              echoed: String(message),
            })
          )
        },
      },
    })
    servers.push(upstream)

    const baseURL = await startWorker(`${upstream.url.origin}/backend-api/codex`)
    const tenant = await createTenant(baseURL, "WebSocket Org")
    await upsertCodexAccount(baseURL, tenant.id, "acct-ws")

    const wsURL = baseURL.replace("http://", "ws://") + "/v1/responses"
    const socket = new WebSocket(wsURL, {
      headers: {
        Authorization: `Bearer ${tenant.key}`,
        "X-Subrouter-Session": "ws-session:turn-0",
        "X-Subrouter-User-Email": "symphony@manaflow.ai",
      },
    } as unknown as string[])
    await waitForWebSocket(socket, "open")
    socket.send("ws-secret")
    const reply = await waitForWebSocket(socket, "message")
    socket.close()

    expect(String(reply.data)).toContain("ws-secret")
    expect(upstreamAuth as string | null).toBe("Bearer codex-access-token")

    const summaries = await waitForTranscriptEvents(baseURL, tenant.id, 3)
    expect(summaries).toHaveLength(1)
    expect(summaries[0]?.session_id).toBe("ws-session")
    expect(summaries[0]?.event_count).toBe(3)
    expect(summaries[0]?.usage.total_tokens).toBe(11)

    const detail = await fetch(
      `${baseURL}/_subrouter/transcripts/codex/ws-session?tenant=${tenant.id}`,
      { headers: { Authorization: `Bearer ${adminToken}` } }
    )
    const detailText = await detail.text()
    expect(detail.status).toBe(200)
    expect(detailText).not.toContain("body_base64\"")
    expect(detailText).toContain("websocket_message")
    expect(detailText).toContain("body_base64_redacted")

    const raw = await fetch(
      `${baseURL}/_subrouter/transcripts/codex/ws-session/raw?tenant=${tenant.id}`,
      { headers: { Authorization: `Bearer ${adminToken}` } }
    )
    const rawText = await raw.text()
    expect(raw.status).toBe(200)
    expect(rawText).toContain("ws-secret")
    expect(rawText).toContain("gpt-5.5")
  }, 60_000)
})

const startWorker = async (codexUpstream: string): Promise<string> => {
  const workerPort = 18_000 + Math.floor(Math.random() * 1_000)
  const persistDir = await mkdtemp(join(tmpdir(), "subrouter-do-worker-test-"))
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
      `PROXY_TOKEN:${proxyToken}`,
      "--var",
      `CODEX_UPSTREAM:${codexUpstream}`,
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
): Promise<{ readonly id: string; readonly name: string; readonly key: string }> => {
  const response = await fetch(`${baseURL}/admin/tenants`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${adminToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ name }),
  })
  expect(response.status).toBe(200)
  const tenant = (await response.json()) as { id: string; name: string; key: string }
  expect(tenant.key).toMatch(/^srt_[0-9a-f]{32}$/)
  return tenant
}

const upsertCodexAccount = async (
  baseURL: string,
  orgId: string,
  accountId: string
): Promise<void> => {
  await upsertAccount(baseURL, {
    id: accountId,
    orgId,
    kind: "codex_oauth",
    label: "lawrence@manaflow.ai",
    credentials: {
      accessToken: "codex-access-token",
    },
  })
}

const upsertAccount = async (
  baseURL: string,
  input: {
    readonly id: string
    readonly orgId: string
    readonly kind: string
    readonly label: string
    readonly credentials: Record<string, unknown>
  }
): Promise<void> => {
  const upsert = await fetch(`${baseURL}/admin/accounts`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${adminToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(input),
  })
  expect(upsert.status).toBe(200)
}

const adminJSON = async <T>(baseURL: string, path: string): Promise<T> => {
  const response = await fetch(`${baseURL}${path}`, {
    headers: { Authorization: `Bearer ${adminToken}` },
  })
  expect(response.status).toBe(200)
  return (await response.json()) as T
}

const waitForTranscriptEvents = async (
  baseURL: string,
  tenantId: string,
  eventCount: number
): Promise<Array<Record<string, any>>> => {
  const deadline = Date.now() + 10_000
  let summaries: Array<Record<string, any>> = []
  while (Date.now() < deadline) {
    const list = await fetch(`${baseURL}/_subrouter/transcripts?tenant=${tenantId}`, {
      headers: { Authorization: `Bearer ${adminToken}` },
    })
    summaries = (await list.json()) as Array<Record<string, any>>
    if ((summaries[0]?.event_count ?? 0) >= eventCount) return summaries
    await Bun.sleep(100)
  }
  return summaries
}

const waitForWebSocket = <Type extends "open" | "message">(
  socket: WebSocket,
  type: Type
): Promise<Type extends "message" ? MessageEvent : Event> =>
  new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error(`websocket ${type} timed out`)), 10_000)
    socket.addEventListener(
      type,
      (event) => {
        clearTimeout(timeout)
        resolve(event as Type extends "message" ? MessageEvent : Event)
      },
      { once: true }
    )
    socket.addEventListener("error", () => {
      clearTimeout(timeout)
      reject(new Error(`websocket ${type} failed`))
    }, { once: true })
  })

const waitForCondition = async (
  condition: () => boolean,
  label: string
): Promise<void> => {
  const deadline = Date.now() + 10_000
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
