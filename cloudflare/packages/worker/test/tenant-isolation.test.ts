import { afterEach, describe, expect, test } from "bun:test"
import {
  adminJSON,
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

    const worker = await startWorker({ claudeUpstream: upstream.url.origin })
    const baseURL = worker.baseURL
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
        agentType: "claude",
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

  test("routes mixed provider accounts by proxy upstream family", async () => {
    const claudeAuths: string[] = []
    const claudeUpstream = Bun.serve({
      port: 0,
      async fetch(request) {
        claudeAuths.push(request.headers.get("Authorization") ?? "")
        return Response.json({ id: "claude-response", type: "message" })
      },
    })
    servers.push(claudeUpstream)
    const codexAuths: string[] = []
    const codexUpstream = Bun.serve({
      port: 0,
      async fetch(request) {
        codexAuths.push(request.headers.get("Authorization") ?? "")
        return Response.json({ id: "codex-response" })
      },
    })
    servers.push(codexUpstream)
    const usage = Bun.serve({
      port: 0,
      async fetch() {
        return Response.json({
          five_hour: { utilization: 1, resets_at: "2026-07-03T12:00:00.000Z" },
        })
      },
    })
    servers.push(usage)

    const worker = await startWorker({
      claudeUpstream: claudeUpstream.url.origin,
      codexUpstream: `${codexUpstream.url.origin}/backend-api/codex`,
    })
    const baseURL = worker.baseURL
    const tenant = await createTenant(baseURL, "Mixed Provider Tenant")
    const claudeAccount = await uploadClaudeAccount(
      baseURL,
      tenant.key,
      "mixed-claude@example.com",
      "mixed-claude-access-token",
      `${usage.url.origin}/usage`
    )
    const codexAccount = await uploadCodexAccount(
      baseURL,
      tenant.key,
      "mixed-codex@example.com",
      "mixed-codex-access-token"
    )

    for (let i = 0; i < 4; i += 1) {
      const response = await fetch(`${baseURL}/v1/messages`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${tenant.key}`,
          "Content-Type": "application/json",
          "X-Subrouter-Session": `mixed-claude-${i}`,
        },
        body: JSON.stringify({ model: "claude-haiku-4", messages: [] }),
      })
      expect(response.status).toBe(200)
    }

    for (let i = 0; i < 4; i += 1) {
      const response = await fetch(`${baseURL}/v1/responses`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${tenant.key}`,
          "Content-Type": "application/json",
          "X-Subrouter-Session": `mixed-codex-${i}`,
        },
        body: JSON.stringify({ model: "gpt-5", input: "hello" }),
      })
      expect(response.status).toBe(200)
    }

    expect(claudeAuths).toEqual([
      "Bearer mixed-claude-access-token",
      "Bearer mixed-claude-access-token",
      "Bearer mixed-claude-access-token",
      "Bearer mixed-claude-access-token",
    ])
    expect(codexAuths).toEqual([
      "Bearer mixed-codex-access-token",
      "Bearer mixed-codex-access-token",
      "Bearer mixed-codex-access-token",
      "Bearer mixed-codex-access-token",
    ])

    const socket = new WebSocket(baseURL.replace("http://", "ws://") + "/ws", {
      headers: { Authorization: `Bearer ${tenant.key}` },
    } as unknown as string[])
    await waitForWebSocket(socket, "open")
    const codexRoute = await sendWebSocketJSON(socket, {
      type: "route",
      requestId: "codex-route",
      sessionId: "ws-route-codex",
      model: "gpt-5",
    })
    const claudeRoute = await sendWebSocketJSON(socket, {
      type: "route",
      requestId: "claude-route",
      agentType: "claude",
      sessionId: "ws-route-claude",
      model: "claude-haiku-4",
    })
    socket.close()
    expect(codexRoute.ok).toBe(true)
    expect(codexRoute.message?.account?.id).toBe(codexAccount.id)
    expect(claudeRoute.ok).toBe(true)
    expect(claudeRoute.message?.account?.id).toBe(claudeAccount.id)
  }, 90_000)
})

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

const uploadCodexAccount = async (
  baseURL: string,
  tenantKey: string,
  label: string,
  accessToken: string
): Promise<Record<string, any>> => {
  const response = await fetch(`${baseURL}/tenant/accounts`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${tenantKey}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      provider: "codex",
      label,
      tokens: {
        accessToken,
        refreshToken: `${accessToken}-refresh`,
        idToken: `${accessToken}-id`,
        accountID: `${accessToken}-account`,
      },
    }),
  })
  expect(response.status).toBe(200)
  const text = await response.text()
  expect(text).not.toContain(accessToken)
  return JSON.parse(text) as Record<string, any>
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

const sendWebSocketJSON = async (
  socket: WebSocket,
  payload: Record<string, unknown>
): Promise<Record<string, any>> => {
  socket.send(JSON.stringify(payload))
  const reply = await waitForWebSocket(socket, "message")
  return JSON.parse(String(reply.data)) as Record<string, any>
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
