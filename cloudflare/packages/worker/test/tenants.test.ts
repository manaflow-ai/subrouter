import { afterEach, describe, expect, test } from "bun:test"
import {
  adminJSON,
  adminToken,
  cleanupWorkers,
  createTenant,
  makePersistDir,
  startWorker,
  stopWorker,
} from "./helpers/worker.ts"

afterEach(async () => {
  await cleanupWorkers()
})

describe("tenant registry", () => {
  test("creates, lists, revokes, rotates, authenticates, and persists tenants", async () => {
    const persistDir = await makePersistDir("subrouter-tenants-test-")
    const first = await startWorker({ persistDir })

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

    await stopWorker(first)
    const second = await startWorker({ persistDir })
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

const sha256Hex = async (value: string): Promise<string> => {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(value)
  )
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("")
}
