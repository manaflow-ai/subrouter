import { DurableObject } from "cloudflare:workers"

export interface TenantRecord {
  readonly id: string
  readonly name: string
  readonly created_at: number
  readonly revoked_at: number | null
}

export interface TenantResolution extends TenantRecord {
  readonly key_sha256: string
}

interface TenantRow {
  readonly [key: string]: SqlStorageValue
  readonly id: string
  readonly name: string
  readonly key_sha256: string
  readonly created_at: number
  readonly revoked_at: number | null
}

const tenantKeyPattern = /^srt_[0-9a-f]{32}$/
const reservedTenantIds = new Set(["legacy", "demo-org"])

export const isTenantKey = (value: string): boolean => tenantKeyPattern.test(value)
export const isReservedTenantId = (value: string): boolean =>
  reservedTenantIds.has(value)

export const createTenantKey = (): string => `srt_${randomHex(16)}`

export const tenantKeySha256Hex = async (key: string): Promise<string> => {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(key)
  )
  return bytesToHex(new Uint8Array(digest))
}

export class TenantRegistryDurableObject extends DurableObject<unknown> {
  constructor(ctx: DurableObjectState, env: unknown) {
    super(ctx, env)
    this.ctx.storage.sql.exec(`
      CREATE TABLE IF NOT EXISTS tenants(
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        key_sha256 TEXT NOT NULL UNIQUE,
        created_at INTEGER NOT NULL,
        revoked_at INTEGER
      );
      CREATE INDEX IF NOT EXISTS tenants_revoked_idx
        ON tenants(revoked_at);
    `)
  }

  async createTenant(input: { readonly name?: unknown }): Promise<TenantRecord & { readonly key: string }> {
    const name = normalizeTenantName(input.name)
    const id = this.uniqueTenantId(name)
    const key = createTenantKey()
    const keySha256 = await tenantKeySha256Hex(key)
    const now = Date.now()
    this.ctx.storage.sql.exec(
      `INSERT INTO tenants(id, name, key_sha256, created_at, revoked_at)
       VALUES (?, ?, ?, ?, NULL)`,
      id,
      name,
      keySha256,
      now
    )
    return { id, name, key, created_at: now, revoked_at: null }
  }

  async listTenants(): Promise<ReadonlyArray<TenantRecord>> {
    return this.ctx.storage.sql
      .exec<TenantRow>(
        `SELECT id, name, key_sha256, created_at, revoked_at
         FROM tenants
         ORDER BY created_at, id`
      )
      .toArray()
      .map(publicTenant)
  }

  async revokeTenant(id: string): Promise<TenantRecord> {
    const tenant = this.getTenant(id)
    if (!tenant) throw new Error("tenant not found")
    const revokedAt = Date.now()
    this.ctx.storage.sql.exec(
      "UPDATE tenants SET revoked_at = ? WHERE id = ?",
      revokedAt,
      tenant.id
    )
    return { ...publicTenant(tenant), revoked_at: revokedAt }
  }

  async rotateTenant(id: string): Promise<TenantRecord & { readonly key: string }> {
    const tenant = this.getTenant(id)
    if (!tenant) throw new Error("tenant not found")
    if (tenant.revoked_at !== null) throw new Error("tenant is revoked")
    const key = createTenantKey()
    const keySha256 = await tenantKeySha256Hex(key)
    this.ctx.storage.sql.exec(
      "UPDATE tenants SET key_sha256 = ? WHERE id = ?",
      keySha256,
      tenant.id
    )
    return { ...publicTenant(tenant), key }
  }

  async resolveTenantKeySha256(keySha256: string): Promise<TenantResolution | null> {
    const row =
      this.ctx.storage.sql
        .exec<TenantRow>(
          `SELECT id, name, key_sha256, created_at, revoked_at
           FROM tenants
           WHERE key_sha256 = ?`,
          keySha256
        )
        .toArray()[0] ?? null
    return row
  }

  private getTenant(id: string): TenantRow | null {
    const normalized = normalizeTenantId(id)
    if (!normalized) throw new Error("invalid tenant id")
    return (
      this.ctx.storage.sql
        .exec<TenantRow>(
          `SELECT id, name, key_sha256, created_at, revoked_at
           FROM tenants
           WHERE id = ?`,
          normalized
        )
        .toArray()[0] ?? null
    )
  }

  private uniqueTenantId(name: string): string {
    const base = slugifyTenantName(name)
    for (let attempt = 0; attempt < 16; attempt += 1) {
      const id = attempt === 0 ? base : `${base}-${randomHex(4)}`
      const exists = this.ctx.storage.sql
        .exec<{ readonly [key: string]: SqlStorageValue; readonly count: number }>(
          "SELECT COUNT(*) AS count FROM tenants WHERE id = ?",
          id
        )
        .one().count
      if (exists === 0 && !isReservedTenantId(id)) return id
    }
    return `tenant-${randomHex(8)}`
  }
}

const publicTenant = (row: TenantRow): TenantRecord => ({
  id: row.id,
  name: row.name,
  created_at: row.created_at,
  revoked_at: row.revoked_at,
})

const normalizeTenantName = (value: unknown): string => {
  if (typeof value !== "string") throw new Error("Missing name")
  const trimmed = value.trim()
  if (!trimmed) throw new Error("Missing name")
  if (trimmed.length > 120) throw new Error("Tenant name is too long")
  return trimmed
}

export const normalizeTenantId = (value: string): string => {
  const trimmed = value.trim()
  if (!trimmed || trimmed.length > 80) return ""
  return /^[a-z0-9][a-z0-9._-]*$/.test(trimmed) ? trimmed : ""
}

const slugifyTenantName = (name: string): string => {
  const slug = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48)
    .replace(/[-._]+$/g, "")
  return normalizeTenantId(slug) || `tenant-${randomHex(4)}`
}

const randomHex = (byteLength: number): string => {
  const bytes = new Uint8Array(byteLength)
  crypto.getRandomValues(bytes)
  return bytesToHex(bytes)
}

const bytesToHex = (bytes: Uint8Array): string =>
  [...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("")
