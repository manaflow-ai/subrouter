import { afterEach, describe, expect, test } from "bun:test"
import {
  adminJSON,
  adminToken,
  cleanupWorkers,
  createTenant,
  proxyToken,
  startWorker,
} from "./helpers/worker.ts"

const servers: Array<{ stop: () => void }> = []

afterEach(async () => {
  await cleanupWorkers()
  for (const server of servers.splice(0)) server.stop()
})

describe("legacy proxy token compatibility", () => {
  test("rejects PROXY_TOKEN by default", async () => {
    const worker = await startWorker({ vars: { PROXY_TOKEN: proxyToken } })
    const baseURL = worker.baseURL
    const response = await fetch(`${baseURL}/status`, {
      headers: { Authorization: `Bearer ${proxyToken}` },
    })
    expect(response.status).toBe(401)
  }, 60_000)

  test("routes PROXY_TOKEN to the legacy tenant only when enabled", async () => {
    let upstreamAuth: string | null = null
    const upstream = Bun.serve({
      port: 0,
      async fetch(request) {
        upstreamAuth = request.headers.get("Authorization")
        return Response.json({ ok: true })
      },
    })
    servers.push(upstream)

    const worker = await startWorker({
      allowLegacy: true,
      apiUpstream: upstream.url.origin,
    })
    const baseURL = worker.baseURL
    await upsertLegacyAccount(baseURL)

    const response = await fetch(`${baseURL}/v1/responses`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${proxyToken}`,
        "Content-Type": "application/json",
        "X-Subrouter-Session": "legacy-session",
      },
      body: JSON.stringify({ model: "gpt-5", input: "hello" }),
    })
    expect(response.status).toBe(200)
    expect(upstreamAuth as string | null).toBe("Bearer sk-legacy")
  }, 60_000)

  test("keeps reserved-name registry tenants isolated from bare legacy objects", async () => {
    const upstreamAuths: string[] = []
    const upstream = Bun.serve({
      port: 0,
      async fetch(request) {
        upstreamAuths.push(request.headers.get("Authorization") ?? "")
        return Response.json({ ok: true })
      },
    })
    servers.push(upstream)

    const worker = await startWorker({
      allowLegacy: true,
      apiUpstream: upstream.url.origin,
    })
    const baseURL = worker.baseURL
    const legacyNamedTenant = await createTenant(baseURL, "legacy")
    const demoNamedTenant = await createTenant(baseURL, "demo-org")
    expect(legacyNamedTenant.id).not.toBe("legacy")
    expect(demoNamedTenant.id).not.toBe("demo-org")

    const registryLegacyAccount = await uploadOpenAIAccount(
      baseURL,
      legacyNamedTenant.key,
      "registry legacy",
      "sk-registry-legacy"
    )
    const registryDemoAccount = await uploadOpenAIAccount(
      baseURL,
      demoNamedTenant.key,
      "registry demo",
      "sk-registry-demo"
    )
    await upsertBareAccount(baseURL, {
      id: "legacy-account",
      orgId: "legacy",
      apiKey: "sk-legacy",
    })
    await upsertBareAccount(baseURL, {
      id: "demo-org-account",
      orgId: "demo-org",
      apiKey: "sk-demo-org",
    })

    const registryLegacyList = await tenantAccounts(baseURL, legacyNamedTenant.key)
    const registryDemoList = await tenantAccounts(baseURL, demoNamedTenant.key)
    const bareLegacyList = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      "/_subrouter/accounts?tenant=legacy"
    )
    const bareDemoList = await adminJSON<Array<Record<string, any>>>(
      baseURL,
      "/_subrouter/accounts?tenant=demo-org"
    )
    expect(registryLegacyList.map((account) => account.id)).toEqual([
      registryLegacyAccount.id,
    ])
    expect(registryDemoList.map((account) => account.id)).toEqual([
      registryDemoAccount.id,
    ])
    expect(bareLegacyList.map((account) => account.id)).toEqual(["legacy-account"])
    expect(bareDemoList.map((account) => account.id)).toEqual(["demo-org-account"])

    const registryLegacyProxy = await fetch(`${baseURL}/v1/responses`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${legacyNamedTenant.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Session": "registry-legacy-session",
      },
      body: "{}",
    })
    const registryDemoProxy = await fetch(`${baseURL}/v1/responses`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${demoNamedTenant.key}`,
        "Content-Type": "application/json",
        "X-Subrouter-Session": "registry-demo-session",
      },
      body: "{}",
    })
    const legacyProxy = await fetch(`${baseURL}/v1/responses`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${proxyToken}`,
        "Content-Type": "application/json",
        "X-Subrouter-Session": "bare-legacy-session",
      },
      body: "{}",
    })
    expect(registryLegacyProxy.status).toBe(200)
    expect(registryDemoProxy.status).toBe(200)
    expect(legacyProxy.status).toBe(200)
    expect(upstreamAuths).toEqual([
      "Bearer sk-registry-legacy",
      "Bearer sk-registry-demo",
      "Bearer sk-legacy",
    ])
  }, 60_000)
})

const upsertLegacyAccount = async (baseURL: string): Promise<void> => {
  await upsertBareAccount(baseURL, {
    id: "legacy-account",
    orgId: "legacy",
    apiKey: "sk-legacy",
  })
}

const uploadOpenAIAccount = async (
  baseURL: string,
  tenantKey: string,
  label: string,
  apiKey: string
): Promise<Record<string, any>> => {
  const response = await fetch(`${baseURL}/tenant/accounts`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${tenantKey}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      provider: "openai-apikey",
      label,
      apiKey,
    }),
  })
  expect(response.status).toBe(200)
  const text = await response.text()
  expect(text).not.toContain(apiKey)
  return JSON.parse(text) as Record<string, any>
}

const tenantAccounts = async (
  baseURL: string,
  tenantKey: string
): Promise<Array<Record<string, any>>> => {
  const response = await fetch(`${baseURL}/tenant/accounts`, {
    headers: { Authorization: `Bearer ${tenantKey}` },
  })
  expect(response.status).toBe(200)
  const body = (await response.json()) as { accounts: Array<Record<string, any>> }
  return body.accounts
}

const upsertBareAccount = async (
  baseURL: string,
  input: {
    readonly id: string
    readonly orgId: string
    readonly apiKey: string
  }
): Promise<void> => {
  const response = await fetch(`${baseURL}/admin/accounts`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${adminToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      id: input.id,
      orgId: input.orgId,
      kind: "openai_apikey",
      label: `${input.id}@example.com`,
      credentials: { apiKey: input.apiKey },
    }),
  })
  expect(response.status).toBe(200)
}
