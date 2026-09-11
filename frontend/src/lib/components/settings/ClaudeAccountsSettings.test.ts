// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { ClaudeAccountInfo } from "../../api/generated/index";

const claudeServiceMocks = vi.hoisted(() => ({
  getApiV1ClaudeAccounts: vi.fn(),
  postApiV1ClaudeAccountsByNameTest: vi.fn(),
}));

vi.mock("../../api/runtime.js", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/runtime.js")>();
  return {
    ...orig,
    callGenerated: vi.fn((request: (options?: { signal?: AbortSignal }) => Promise<unknown>) =>
      request(),
    ),
  };
});

vi.mock("../../api/generated/index", async (importOriginal) => {
  const orig = await importOriginal<typeof import("../../api/generated/index")>();
  return {
    ...orig,
    ClaudeService: claudeServiceMocks,
  };
});

const { claudeAccounts } = await import("../../stores/claudeAccounts.svelte.js");
const { default: ClaudeAccountsSettings } = await import("./ClaudeAccountsSettings.svelte");

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

let component: ReturnType<typeof mount> | undefined;

afterEach(() => {
  if (component) {
    void unmount(component);
    component = undefined;
  }
  vi.clearAllMocks();
  claudeAccounts.accounts = [];
  claudeAccounts.loaded = false;
  claudeAccounts.error = null;
  claudeAccounts.testing = {};
  claudeAccounts.testResult = {};
  document.body.innerHTML = "";
});

describe("ClaudeAccountsSettings", () => {
  it("shows an empty state with no configured accounts", async () => {
    claudeServiceMocks.getApiV1ClaudeAccounts.mockResolvedValue({ accounts: [] });

    component = mount(ClaudeAccountsSettings, { target: document.body });
    await tick();
    await tick();

    expect(document.body.textContent).toContain("No Claude accounts configured");
  });

  it("renders one row per configured account with identity and tier", async () => {
    claudeServiceMocks.getApiV1ClaudeAccounts.mockResolvedValue({ accounts: [account()] });

    component = mount(ClaudeAccountsSettings, { target: document.body });
    await tick();
    await tick();

    expect(document.querySelector(".account-name")?.textContent).toBe("personal");
    expect(document.querySelector(".account-label")?.textContent).toBe("Acme Org");
    expect(document.querySelector(".account-tier")?.textContent).toBe("default_claude_max_5x");
  });

  it("shows a warning when identity is unavailable", async () => {
    claudeServiceMocks.getApiV1ClaudeAccounts.mockResolvedValue({
      accounts: [account({ identityAvailable: false, identityError: "not logged in", label: undefined })],
    });

    component = mount(ClaudeAccountsSettings, { target: document.body });
    await tick();
    await tick();

    expect(document.querySelector(".account-no-identity")?.textContent).toContain("not logged in");
  });

  it("clicking Test calls the trigger endpoint and refreshes on success", async () => {
    claudeServiceMocks.getApiV1ClaudeAccounts.mockResolvedValue({ accounts: [account()] });
    claudeServiceMocks.postApiV1ClaudeAccountsByNameTest.mockResolvedValue({ success: true });

    component = mount(ClaudeAccountsSettings, { target: document.body });
    await tick();
    await tick();

    const button = [...document.querySelectorAll("button")].find((b) =>
      b.textContent?.includes("Test"),
    );
    expect(button).toBeDefined();
    button!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await tick();
    await tick();
    await tick();

    expect(claudeServiceMocks.postApiV1ClaudeAccountsByNameTest).toHaveBeenCalledWith({
      name: "personal",
    });
  });

  it("shows the last error reported by the poller", async () => {
    claudeServiceMocks.getApiV1ClaudeAccounts.mockResolvedValue({
      accounts: [account({ lastError: "401 Unauthorized" })],
    });

    component = mount(ClaudeAccountsSettings, { target: document.body });
    await tick();
    await tick();

    expect(document.body.textContent).toContain("401 Unauthorized");
  });
});
