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

describe("legacy proxy token compatibility", () => {
  test("rejects PROXY_TOKEN by default", async () => {
    const baseURL = await startWorker({ allowLegacy: false })
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

    const baseURL = await startWorker({
      allowLegacy: true,
      apiUpstream: upstream.url.origin,
    })
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

    const baseURL = await startWorker({
      allowLegacy: true,
      apiUpstream: upstream.url.origin,
    })
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

const startWorker = async (input: {
  readonly allowLegacy: boolean
  readonly apiUpstream?: string
}): Promise<string> => {
  const workerPort = 22_000 + Math.floor(Math.random() * 1_000)
  const persistDir = await mkdtemp(join(tmpdir(), "subrouter-legacy-test-"))
  const args = [
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
  ]
  if (input.allowLegacy) {
    args.push("--var", "ALLOW_LEGACY_PROXY_TOKEN:1")
  }
  if (input.apiUpstream) {
    args.push("--var", `API_UPSTREAM:${input.apiUpstream}`)
  }
  args.push("--log-level", "error")
  const worker = Bun.spawn(args, {
    cwd: import.meta.dir.replace(/\/test$/, ""),
    stdout: "pipe",
    stderr: "pipe",
  })
  processes.push(worker)
  const baseURL = `http://127.0.0.1:${workerPort}`
  await waitForWorker(baseURL)
  return baseURL
}

const upsertLegacyAccount = async (baseURL: string): Promise<void> => {
  await upsertBareAccount(baseURL, {
    id: "legacy-account",
    orgId: "legacy",
    apiKey: "sk-legacy",
  })
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

const adminJSON = async <T>(baseURL: string, path: string): Promise<T> => {
  const response = await fetch(`${baseURL}${path}`, {
    headers: { Authorization: `Bearer ${adminToken}` },
  })
  expect(response.status).toBe(200)
  return (await response.json()) as T
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
