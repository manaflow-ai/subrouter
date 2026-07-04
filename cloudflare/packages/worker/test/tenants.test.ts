import { afterEach, describe, expect, test } from "bun:test"
import { mkdtemp } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"

const adminToken = "admin-test-token"

const processes: Bun.Subprocess[] = []

afterEach(async () => {
  for (const proc of processes.splice(0)) {
    proc.kill()
    await proc.exited.catch(() => {})
  }
})

describe("tenant registry", () => {
  test("creates, lists, revokes, rotates, authenticates, and persists tenants", async () => {
    const persistDir = await mkdtemp(join(tmpdir(), "subrouter-tenants-test-"))
    const first = await startWorker(persistDir)

    const createA = await createTenant(first.baseURL, "Tenant Alpha")
    const createText = JSON.stringify(createA)
    expect(createA.key).toMatch(/^srt_[0-9a-f]{32}$/)
    expect(createText).toContain(createA.key)

    const createB = await createTenant(first.baseURL, "Tenant Beta")
    const listBefore = await adminJSON<Array<Record<string, unknown>>>(
      first.baseURL,
      "/admin/tenants"
    )
    const listText = JSON.stringify(listBefore)
    expect(listBefore.map((tenant) => tenant.id)).toEqual([
      createA.id,
      createB.id,
    ])
    expect(listText).not.toContain(createA.key)
    expect(listText).not.toContain(await sha256Hex(createA.key))

    expect((await fetch(`${first.baseURL}/status`)).status).toBe(401)
    expect(
      (
        await fetch(`${first.baseURL}/status`, {
          headers: { Authorization: "Bearer srt_not-a-valid-key" },
        })
      ).status
    ).toBe(401)
    expect(
      (
        await fetch(`${first.baseURL}/status`, {
          headers: { Authorization: `Bearer ${createA.key}` },
        })
      ).status
    ).toBe(200)

    const revoke = await fetch(`${first.baseURL}/admin/tenants/${createA.id}/revoke`, {
      method: "POST",
      headers: { Authorization: `Bearer ${adminToken}` },
    })
    expect(revoke.status).toBe(200)
    const revokeText = await revoke.text()
    expect(revokeText).not.toContain(createA.key)
    expect(
      (
        await fetch(`${first.baseURL}/status`, {
          headers: { Authorization: `Bearer ${createA.key}` },
        })
      ).status
    ).toBe(403)

    const rotate = await fetch(`${first.baseURL}/admin/tenants/${createB.id}/rotate`, {
      method: "POST",
      headers: { Authorization: `Bearer ${adminToken}` },
    })
    expect(rotate.status).toBe(200)
    const rotated = (await rotate.json()) as { key: string }
    expect(rotated.key).toMatch(/^srt_[0-9a-f]{32}$/)
    expect(rotated.key).not.toBe(createB.key)
    expect(
      (
        await fetch(`${first.baseURL}/status`, {
          headers: { Authorization: `Bearer ${createB.key}` },
        })
      ).status
    ).toBe(401)
    expect(
      (
        await fetch(`${first.baseURL}/status`, {
          headers: { "X-Subrouter-Tenant-Key": rotated.key },
        })
      ).status
    ).toBe(200)

    await stopWorker(first.proc)
    const second = await startWorker(persistDir)
    const listAfter = await adminJSON<Array<Record<string, unknown>>>(
      second.baseURL,
      "/admin/tenants"
    )
    expect(listAfter.map((tenant) => tenant.id)).toEqual([
      createA.id,
      createB.id,
    ])
    expect(JSON.stringify(listAfter)).not.toContain(createA.key)
    expect(JSON.stringify(listAfter)).not.toContain(rotated.key)
  }, 90_000)
})

const startWorker = async (
  persistDir: string
): Promise<{ readonly baseURL: string; readonly proc: Bun.Subprocess }> => {
  const workerPort = 19_000 + Math.floor(Math.random() * 1_000)
  const proc = Bun.spawn(
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
      "--log-level",
      "error",
    ],
    {
      cwd: import.meta.dir.replace(/\/test$/, ""),
      stdout: "pipe",
      stderr: "pipe",
    }
  )
  processes.push(proc)
  const baseURL = `http://127.0.0.1:${workerPort}`
  await waitForWorker(baseURL)
  return { baseURL, proc }
}

const stopWorker = async (proc: Bun.Subprocess): Promise<void> => {
  const index = processes.indexOf(proc)
  if (index >= 0) processes.splice(index, 1)
  proc.kill()
  await proc.exited.catch(() => {})
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
  return (await response.json()) as { id: string; name: string; key: string }
}

const adminJSON = async <T>(baseURL: string, path: string): Promise<T> => {
  const response = await fetch(`${baseURL}${path}`, {
    headers: { Authorization: `Bearer ${adminToken}` },
  })
  expect(response.status).toBe(200)
  return (await response.json()) as T
}

const sha256Hex = async (value: string): Promise<string> => {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(value)
  )
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("")
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
