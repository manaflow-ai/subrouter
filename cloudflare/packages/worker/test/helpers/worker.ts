import { expect } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"

export const adminToken = "admin-test-token"
export const proxyToken = "proxy-test-token"

export interface StartedWorker {
  readonly baseURL: string
  readonly proc: Bun.Subprocess
  readonly persistDir: string
}

export interface StartWorkerOptions {
  readonly codexUpstream?: string
  readonly apiUpstream?: string
  readonly claudeUpstream?: string
  readonly allowLegacy?: boolean
  readonly persistDir?: string
  readonly vars?: Readonly<Record<string, string | number | boolean>>
}

const workers: StartedWorker[] = []
const tempDirs = new Set<string>()

export const makePersistDir = async (prefix: string): Promise<string> => {
  const persistDir = await mkdtemp(join(tmpdir(), prefix))
  tempDirs.add(persistDir)
  return persistDir
}

export const startWorker = async (
  options: StartWorkerOptions = {}
): Promise<StartedWorker> => {
  const workerPort = 18_000 + Math.floor(Math.random() * 20_000)
  const persistDir =
    options.persistDir ?? await makePersistDir("subrouter-worker-test-")
  tempDirs.add(persistDir)
  const vars: Record<string, string | number | boolean> = {
    ADMIN_TOKEN: adminToken,
    ...(options.codexUpstream ? { CODEX_UPSTREAM: options.codexUpstream } : {}),
    ...(options.apiUpstream ? { API_UPSTREAM: options.apiUpstream } : {}),
    ...(options.claudeUpstream ? { CLAUDE_UPSTREAM: options.claudeUpstream } : {}),
    ...(options.allowLegacy ? { PROXY_TOKEN: proxyToken, ALLOW_LEGACY_PROXY_TOKEN: "1" } : {}),
    ...(options.vars ?? {}),
  }
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
  ]
  for (const [name, value] of Object.entries(vars)) {
    args.push("--var", `${name}:${String(value)}`)
  }
  args.push("--log-level", "error")
  const proc = Bun.spawn(args, {
    cwd: import.meta.dir.replace(/\/test\/helpers$/, ""),
    stdout: "pipe",
    stderr: "pipe",
  })
  const worker = { baseURL: `http://127.0.0.1:${workerPort}`, proc, persistDir }
  workers.push(worker)
  await waitForWorker(worker.baseURL)
  return worker
}

export const stopWorker = async (worker: StartedWorker): Promise<void> => {
  const index = workers.indexOf(worker)
  if (index >= 0) workers.splice(index, 1)
  await killProcessTree(worker.proc.pid)
  await worker.proc.exited.catch(() => {})
}

export const cleanupWorkers = async (): Promise<void> => {
  for (const worker of workers.splice(0)) {
    await killProcessTree(worker.proc.pid)
    await worker.proc.exited.catch(() => {})
  }
  for (const dir of [...tempDirs]) {
    await rm(dir, { recursive: true, force: true })
    tempDirs.delete(dir)
  }
}

export const createTenant = async (
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

export const adminJSON = async <T>(baseURL: string, path: string): Promise<T> => {
  const response = await fetch(`${baseURL}${path}`, {
    headers: { Authorization: `Bearer ${adminToken}` },
  })
  expect(response.status).toBe(200)
  return (await response.json()) as T
}

export const waitForCondition = async (
  condition: () => boolean,
  label: string,
  timeoutMs = 10_000
): Promise<void> => {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (condition()) return
    await Bun.sleep(100)
  }
  throw new Error(`${label} timed out`)
}

export const waitForWorker = async (baseURL: string): Promise<void> => {
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

const killProcessTree = async (pid: number): Promise<void> => {
  const children = await childPids(pid)
  for (const child of children) {
    await killProcessTree(child)
  }
  signalProcess(pid, "SIGTERM")
  await Bun.sleep(300)
  signalProcess(pid, "SIGKILL")
}

const childPids = async (pid: number): Promise<number[]> => {
  const proc = Bun.spawn(["pgrep", "-P", String(pid)], {
    stdout: "pipe",
    stderr: "ignore",
  })
  const output = await new Response(proc.stdout).text()
  await proc.exited.catch(() => {})
  return output
    .split(/\s+/)
    .map((value) => Number(value))
    .filter((value) => Number.isInteger(value) && value > 0)
}

const signalProcess = (pid: number, signal: NodeJS.Signals): void => {
  try {
    process.kill(pid, signal)
  } catch {
    // The process may already be gone.
  }
}
