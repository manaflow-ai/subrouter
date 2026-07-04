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

describe("tenant isolation", () => {
  test("keeps accounts, sessions, usage status, and transcripts tenant-scoped", async () => {
    const upstreamAuths: string[] = []
    const upstream = Bun.serve({
      port: 0,
      async fetch(request) {
        upstreamAuths.push(request.headers.get("Authorization") ?? "")
        return Response.json({
          response: {
            model: "claude-3-haiku",
            usage: {
              input_tokens: 12,
              output_tokens: 4,
              total_tokens: 16,
            },
          },
        })
      },
    })
    servers.push(upstream)
    const usage = Bun.serve({
      port: 0,
      async fetch() {
        return Response.json({
          five_hour: { utilization: 10, resets_at: "2026-07-03T12:00:00.000Z" },
        })
      },
    })
    servers.push(usage)

    const baseURL = await startWorker(upstream.url.origin)
    const tenantA = await createTenant(baseURL, "Tenant A")
    const tenantB = await createTenant(baseURL, "Tenant B")
    const accountA = await uploadClaudeAccount(
      baseURL,
      tenantA.key,
      "tenant-a@example.com",
      "tenant-a-access-token",
      `${usage.url.origin}/usage`
    )
    const accountB = await uploadClaudeAccount(
      baseURL,
      tenantB.key,
      "tenant-b@example.com",
      "tenant-b-access-token",
      `${usage.url.origin}/usage`
    )

    const proxyA = await fetch(`${baseURL}/v1/messages`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenantA.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Agent": "claude",
        "X-Subrouter-Session": "shared-session",
        "X-Subrouter-User-Email": "a@example.com",
      },
      body: JSON.stringify({ model: "claude-3-haiku", input: "tenant-a-secret" }),
    })
    expect(proxyA.status).toBe(200)

    const proxyB = await fetch(`${baseURL}/v1/messages`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenantB.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Agent": "claude",
        "X-Subrouter-Session": "shared-session",
        "X-Subrouter-User-Email": "b@example.com",
      },
      body: JSON.stringify({ model: "claude-3-haiku", input: "tenant-b-secret" }),
    })
    expect(proxyB.status).toBe(200)
    expect(upstreamAuths).toEqual([
      "Bearer tenant-a-access-token",
      "Bearer tenant-b-access-token",
    ])

    const sessionsA = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/sessions?tenant=${tenantA.id}`
    )
    const sessionsB = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/sessions?tenant=${tenantB.id}`
    )
    expect(sessionsA.map((session) => session.account_id)).toEqual([accountA.id])
    expect(sessionsB.map((session) => session.account_id)).toEqual([accountB.id])

    const usageA = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/usage-status?tenant=${tenantA.id}`
    )
    const usageB = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/usage-status?tenant=${tenantB.id}`
    )
    expect(usageA.map((account) => account.id)).toEqual([accountA.id])
    expect(usageB.map((account) => account.id)).toEqual([accountB.id])
    expect(JSON.stringify(usageB)).not.toContain(accountA.id)

    const transcriptsA = await waitForTranscriptEvents(baseURL, tenantA.id, 3)
    const transcriptsB = await waitForTranscriptEvents(baseURL, tenantB.id, 3)
    expect(transcriptsA).toHaveLength(1)
    expect(transcriptsB).toHaveLength(1)
    expect(transcriptsA[0]?.account).toBe(accountA.id)
    expect(transcriptsB[0]?.account).toBe(accountB.id)
    expect(JSON.stringify(transcriptsB)).not.toContain(accountA.id)

    const rawB = await fetch(
      `${baseURL}/_subrouter/transcripts/claude/shared-session/raw?tenant=${tenantB.id}`,
      { headers: { Authorization: `Bearer ${adminToken}` } }
    )
    const rawBText = await rawB.text()
    expect(rawB.status).toBe(200)
    expect(rawBText).toContain("tenant-b-secret")
    expect(rawBText).not.toContain("tenant-a-secret")
    expect(rawBText).not.toContain(tenantA.key)
    expect(rawBText).not.toContain("tenant-a-access-token")

    const overrideProxy = await fetch(`${baseURL}/v1/messages`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenantA.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Agent": "claude",
        "X-Subrouter-Org-ID": tenantB.id,
        "X-Subrouter-Session": "attacker-proxy-session",
      },
      body: JSON.stringify({
        model: "claude-3-haiku",
        input: "tenant-a-attacker-override",
      }),
    })
    expect(overrideProxy.status).toBe(200)
    expect(upstreamAuths[upstreamAuths.length - 1]).toBe(
      "Bearer tenant-a-access-token"
    )

    const overrideRoute = await fetch(`${baseURL}/route`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${tenantA.key}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        orgId: tenantB.id,
        sessionId: "attacker-route-session",
        model: "claude-3-haiku",
      }),
    })
    expect(overrideRoute.status).toBe(200)
    const overrideRouteBody = (await overrideRoute.json()) as Record<string, any>
    expect(overrideRouteBody.account.id).toBe(accountA.id)

    const sessionsBAfterOverride = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/sessions?tenant=${tenantB.id}`
    )
    const usageBAfterOverride = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/usage-status?tenant=${tenantB.id}`
    )
    const transcriptsBAfterOverride = await waitForTranscriptEvents(
      baseURL,
      tenantB.id,
      3
    )
    expect(sessionsBAfterOverride).toEqual(sessionsB)
    expect(usageBAfterOverride).toEqual(usageB)
    expect(transcriptsBAfterOverride).toEqual(transcriptsB)

    const rawBAfterOverride = await fetch(
      `${baseURL}/_subrouter/transcripts/claude/shared-session/raw?tenant=${tenantB.id}`,
      { headers: { Authorization: `Bearer ${adminToken}` } }
    )
    const rawBAfterOverrideText = await rawBAfterOverride.text()
    expect(rawBAfterOverride.status).toBe(200)
    expect(rawBAfterOverrideText).not.toContain("tenant-a-attacker-override")
  }, 90_000)
})

const startWorker = async (upstreamOrigin: string): Promise<string> => {
  const workerPort = 21_000 + Math.floor(Math.random() * 1_000)
  const persistDir = await mkdtemp(join(tmpdir(), "subrouter-isolation-test-"))
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

const uploadClaudeAccount = async (
  baseURL: string,
  tenantKey: string,
  label: string,
  accessToken: string,
  usageUrl: string
): Promise<Record<string, any>> => {
  const response = await fetch(`${baseURL}/tenant/accounts`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${tenantKey}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      provider: "claude",
      label,
      claudeAiOauth: {
        accessToken,
        refreshToken: `${accessToken}-refresh`,
        expiresAt: Date.now() + 3_600_000,
        subscriptionType: "max",
        usageUrl,
      },
    }),
  })
  expect(response.status).toBe(200)
  const text = await response.text()
  expect(text).not.toContain(accessToken)
  return JSON.parse(text) as Record<string, any>
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
    summaries = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      `/_subrouter/transcripts?tenant=${tenantId}`
    )
    if ((summaries[0]?.event_count ?? 0) >= eventCount) return summaries
    await Bun.sleep(100)
  }
  return summaries
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
