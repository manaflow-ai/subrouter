import { describe, expect, test } from "bun:test"
import {
  accountStatus,
  credentialNeedsRefresh,
  fetchProviderUsage,
  refreshOAuthCredentials,
  parseProxyRouteInput,
  safeGoAccount,
  scopedSessionKey,
  setUpstreamAuthHeaders,
  upstreamURLForRequest,
  usageStatus,
  type StoredAccountContract,
} from "../src/contract.ts"

const jwt = (expiresAtSeconds: number): string => {
  const payload = btoa(JSON.stringify({ exp: expiresAtSeconds }))
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replaceAll("=", "")
  return `header.${payload}.signature`
}

const codexAccount: StoredAccountContract = {
  id: "codex-a",
  orgId: "org-a",
  kind: "codex_oauth",
  label: "alice@example.com",
  enabled: true,
  hasCredentials: true,
  hasTotp: false,
  credentials: {
    accessToken: jwt(1_700_000_000),
    accountId: "chatgpt-account",
    baseUrl: "https://chatgpt.example/backend-api/codex",
  },
  modelQuotas: {
    default: { remainingPercent: 75 },
  },
}

describe("subrouter Durable Object contract", () => {
  test("parses tenant, user, account, model, and codex base session from proxy request", async () => {
    const request = new Request("https://subrouter.cmux.dev/v1/responses", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Subrouter-Org-ID": "org-a",
        "X-Subrouter-Session": "session-1:turn-7",
        "X-Subrouter-User-Email": "Alice <ALICE@example.com>",
        "X-Subrouter-Account-ID": " codex-a ",
      },
      body: JSON.stringify({ model: "gpt-5.3-codex-spark" }),
    })

    await expect(parseProxyRouteInput(request)).resolves.toEqual({
      orgId: "org-a",
      agentType: "codex",
      sessionId: "session-1",
      userEmail: "alice@example.com",
      preferAccountId: "codex-a",
      model: "gpt-5.3-codex-spark",
    })
  })

  test("keeps claude session ids distinct from codex base-session collapsing", async () => {
    expect(scopedSessionKey("codex", "shared:turn-1")).toBe("codex:shared")
    expect(scopedSessionKey("claude", "shared:turn-1")).toBe("claude:shared:turn-1")
  })

  test("fallback sessions use Go-shaped client hash rather than a broad path key", async () => {
    const first = await parseProxyRouteInput(
      new Request("https://subrouter.cmux.dev/v1/responses", {
        method: "POST",
        headers: {
          "CF-Connecting-IP": "203.0.113.1",
          "User-Agent": "client-a",
        },
      })
    )
    const second = await parseProxyRouteInput(
      new Request("https://subrouter.cmux.dev/v1/responses", {
        method: "POST",
        headers: {
          "CF-Connecting-IP": "203.0.113.2",
          "User-Agent": "client-a",
        },
      })
    )

    expect(first.sessionId).toMatch(/^fallback:[a-f0-9]{24}$/)
    expect(second.sessionId).toMatch(/^fallback:[a-f0-9]{24}$/)
    expect(first.sessionId).not.toBe(second.sessionId)
  })

  test("safe account and status payloads never expose credentials", () => {
    expect(JSON.stringify(safeGoAccount(codexAccount))).not.toContain(codexAccount.credentials?.accessToken)
    expect(JSON.stringify(accountStatus(codexAccount))).not.toContain(codexAccount.credentials?.accessToken)
    expect(JSON.stringify(usageStatus(codexAccount))).not.toContain(codexAccount.credentials?.accessToken)
  })

  test("upstream auth strips subrouter routing headers before forwarding", () => {
    const headers = setUpstreamAuthHeaders(
      new Headers({
        "X-Subrouter-Session": "session-1",
        "X-Subrouter-Org-ID": "org-a",
        "X-Subrouter-Account-ID": "codex-a",
        "X-Subrouter-Tenant-Key": "srt_0123456789abcdef0123456789abcdef",
        Authorization: "Bearer caller-token",
      }),
      codexAccount
    )

    expect(headers.get("X-Subrouter-Session")).toBeNull()
    expect(headers.get("X-Subrouter-Org-ID")).toBeNull()
    expect(headers.get("X-Subrouter-Account-ID")).toBeNull()
    expect(headers.get("X-Subrouter-Tenant-Key")).toBeNull()
    expect(headers.get("Authorization")).toBe(`Bearer ${codexAccount.credentials?.accessToken}`)
    expect(headers.get("ChatGPT-Account-ID")).toBe("chatgpt-account")
  })

  test("openai api-key requests keep one v1 prefix", () => {
    const account: StoredAccountContract = {
      ...codexAccount,
      kind: "openai_apikey",
      credentials: {
        apiKey: "sk-test",
        baseUrl: "https://api.openai.com/v1",
      },
    }

    expect(
      upstreamURLForRequest(
        "https://subrouter.cmux.dev/v1/responses",
        account,
        "https://api.openai.com/v1"
      ).toString()
    ).toBe("https://api.openai.com/v1/responses")
  })

  test("codex oauth refresh rotates access, refresh, and id tokens", async () => {
    const now = 1_780_000_000_000
    const expired = jwt(Math.floor((now - 1_000) / 1000))
    const fresh = jwt(Math.floor((now + 600_000) / 1000))
    expect(credentialNeedsRefresh({ accessToken: expired, refreshToken: "old-refresh" }, now)).toBe(true)

    const calls: Request[] = []
    const fetcher = async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push(new Request(input, init))
      return new Response(
        JSON.stringify({
          access_token: fresh,
          refresh_token: "new-refresh",
          id_token: "new-id",
        }),
        { status: 200 }
      )
    }

    const refreshed = await refreshOAuthCredentials(
      "codex_oauth",
      { accessToken: expired, refreshToken: "old-refresh" },
      false,
      fetcher as typeof fetch,
      now
    )

    expect(refreshed.refreshed).toBe(true)
    expect(refreshed.credentials.accessToken).toBe(fresh)
    expect(refreshed.credentials.refreshToken).toBe("new-refresh")
    expect(refreshed.credentials.idToken).toBe("new-id")
    expect(calls[0]?.headers.get("Content-Type")).toBe("application/json")
  })

  test("claude oauth refresh sends form body and anthropic beta header", async () => {
    const now = 1_780_000_000_000
    const calls: Request[] = []
    const fetcher = async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push(new Request(input, init))
      return new Response(
        JSON.stringify({
          access_token: "new-claude-access",
          refresh_token: "new-claude-refresh",
          expires_in: 3600,
        }),
        { status: 200 }
      )
    }

    const refreshed = await refreshOAuthCredentials(
      "anthropic_oauth",
      {
        accessToken: "old-claude-access",
        refreshToken: "old-claude-refresh",
        expiresAt: now - 1,
      },
      false,
      fetcher as typeof fetch,
      now
    )

    const body = await calls[0]!.text()
    expect(calls[0]?.headers.get("anthropic-beta")).toBe("oauth-2025-04-20")
    expect(body).toContain("grant_type=refresh_token")
    expect(body).toContain("refresh_token=old-claude-refresh")
    expect(refreshed.credentials.accessToken).toBe("new-claude-access")
    expect(refreshed.credentials.refreshToken).toBe("new-claude-refresh")
    expect(refreshed.credentials.expiresAt).toBe(now + 3_600_000)
  })

  test("codex usage fetch parses base and model-family windows", async () => {
    const calls: Request[] = []
    const fetcher = async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push(new Request(input, init))
      return new Response(
        JSON.stringify({
          plan_type: "plus",
          rate_limit: {
            primary_window: {
              used_percent: 25,
              limit_window_seconds: 18000,
              reset_after_seconds: 120,
            },
          },
          credits: { has_credits: true, unlimited: false, balance: "1.23" },
          additional_rate_limits: [
            {
              limit_name: "GPT-5.3-Codex-Spark",
              rate_limit: {
                secondary_window: {
                  used_percent: 90,
                  limit_window_seconds: 604800,
                  reset_after_seconds: 3600,
                },
              },
            },
          ],
        }),
        { status: 200 }
      )
    }

    const usage = await fetchProviderUsage(
      "codex_oauth",
      { accessToken: "codex-access", accountId: "chatgpt-account", usageUrl: "https://usage.example" },
      fetcher as typeof fetch
    )

    expect(calls[0]?.headers.get("Authorization")).toBe("Bearer codex-access")
    expect(calls[0]?.headers.get("ChatGPT-Account-ID")).toBe("chatgpt-account")
    expect(usage.plan_type).toBe("plus")
    expect(usage.credits?.balance).toBe("1.23")
    expect(usage.windows?.map((window) => window.name)).toEqual([
      "primary",
      "GPT-5.3-Codex-Spark/secondary",
    ])
    expect(usage.windows?.[1]?.feature).toBe("GPT-5.3-Codex-Spark")
  })

  test("claude usage fetch maps oauth usage windows", async () => {
    const calls: Request[] = []
    const fetcher = async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push(new Request(input, init))
      return new Response(
        JSON.stringify({
          five_hour: { utilization: 12, resets_at: "2026-06-02T13:00:00.000Z" },
          seven_day_opus: { utilization: 34, resets_at: "2026-06-03T13:00:00.000Z" },
          seven_day_sonnet: { utilization: 56, resets_at: "2026-06-04T13:00:00.000Z" },
        }),
        { status: 200 }
      )
    }

    const usage = await fetchProviderUsage(
      "anthropic_oauth",
      { accessToken: "claude-access", usageUrl: "https://usage.example" },
      fetcher as typeof fetch
    )

    expect(calls[0]?.headers.get("Authorization")).toBe("Bearer claude-access")
    expect(calls[0]?.headers.get("anthropic-beta")).toBe("oauth-2025-04-20")
    expect(usage.plan_type).toBe("claude")
    expect(usage.windows?.map((window) => window.name)).toEqual([
      "5h",
      "opus-weekly",
      "sonnet-weekly",
    ])
  })
})
