import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { ClaudeAccountInfo } from "../api/generated/index";

const claudeServiceMocks = vi.hoisted(() => ({
  getApiV1ClaudeAccounts: vi.fn(),
  postApiV1ClaudeAccountsByNameTest: vi.fn(),
}));

vi.mock("../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: (options?: { signal?: AbortSignal }) => Promise<unknown>) =>
      request(),
    ),
  };
});

vi.mock("../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../api/generated/index")>();
  return {
    ...orig,
    ClaudeService: claudeServiceMocks,
  };
});

function account(overrides: Partial<ClaudeAccountInfo> = {}): ClaudeAccountInfo {
  return {
    name: "personal",
    credentialsSource: "keychain",
    identityAvailable: true,
    label: "Acme Org",
    seatTier: "team_tier_1",
    userRateLimitTier: "default_claude_max_5x",
    ...overrides,
  };
}

let claudeAccounts: (typeof import("./claudeAccounts.svelte.js"))["claudeAccounts"];

beforeEach(async () => {
  vi.clearAllMocks();
  ({ claudeAccounts } = await import("./claudeAccounts.svelte.js"));
  claudeAccounts.accounts = [];
  claudeAccounts.loaded = false;
  claudeAccounts.error = null;
  claudeAccounts.testing = {};
  claudeAccounts.testResult = {};
});

describe("claudeAccounts store", () => {
  it("fetch populates accounts", async () => {
    claudeServiceMocks.getApiV1ClaudeAccounts.mockResolvedValue({ accounts: [account()] });

    await claudeAccounts.fetch();

    expect(claudeAccounts.accounts).toHaveLength(1);
    expect(claudeAccounts.accounts[0]?.name).toBe("personal");
    expect(claudeAccounts.error).toBeNull();
    expect(claudeAccounts.loaded).toBe(true);
  });

  it("fetch records an error on failure", async () => {
    claudeServiceMocks.getApiV1ClaudeAccounts.mockRejectedValue(new Error("boom"));

    await claudeAccounts.fetch();

    expect(claudeAccounts.error).toBe("boom");
    expect(claudeAccounts.loaded).toBe(true);
  });

  it("test triggers the account and refetches on success", async () => {
    claudeServiceMocks.postApiV1ClaudeAccountsByNameTest.mockResolvedValue({ success: true });
    claudeServiceMocks.getApiV1ClaudeAccounts.mockResolvedValue({ accounts: [account()] });

    await claudeAccounts.test("personal");

    expect(claudeServiceMocks.postApiV1ClaudeAccountsByNameTest).toHaveBeenCalledWith({
      name: "personal",
    });
    expect(claudeAccounts.testResult.personal).toEqual({ success: true });
    expect(claudeServiceMocks.getApiV1ClaudeAccounts).toHaveBeenCalledTimes(1);
    expect(claudeAccounts.testing.personal).toBe(false);
  });

  it("test records a failure result without refetching", async () => {
    claudeServiceMocks.postApiV1ClaudeAccountsByNameTest.mockResolvedValue({
      success: false,
      error: "401 Unauthorized",
    });

    await claudeAccounts.test("personal");

    expect(claudeAccounts.testResult.personal).toEqual({
      success: false,
      error: "401 Unauthorized",
    });
    expect(claudeServiceMocks.getApiV1ClaudeAccounts).not.toHaveBeenCalled();
  });

  it("test ignores a call while one is already in flight for that account", async () => {
    let resolveFirst: (() => void) | undefined;
    claudeServiceMocks.postApiV1ClaudeAccountsByNameTest.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveFirst = () => resolve({ success: true });
        }),
    );
    claudeServiceMocks.getApiV1ClaudeAccounts.mockResolvedValue({ accounts: [] });

    const first = claudeAccounts.test("personal");
    expect(claudeAccounts.testing.personal).toBe(true);
    await claudeAccounts.test("personal");
    expect(claudeServiceMocks.postApiV1ClaudeAccountsByNameTest).toHaveBeenCalledTimes(1);

    resolveFirst?.();
    await first;
  });
});
