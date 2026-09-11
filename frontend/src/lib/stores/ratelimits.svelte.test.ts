import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import type { ServiceRateLimitWindow } from "../api/generated/index";
import type { RateLimitCardIdentity } from "./ratelimits.svelte.js";

const rateLimitsServiceMocks = vi.hoisted(() => ({
  getApiV1RateLimitsCurrent: vi.fn(),
  getApiV1RateLimitsHistory: vi.fn(),
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
    RateLimitsService: rateLimitsServiceMocks,
  };
});

vi.mock("./sessions.svelte.js", () => ({
  sessions: { filters: { machine: "", agent: "" } },
}));

function codexSnapshot(overrides: Partial<ServiceRateLimitWindow> = {}): ServiceRateLimitWindow {
  return {
    vendor: "codex",
    machine: "laptop",
    limitId: "codex",
    planType: "pro",
    windowKind: "primary",
    usedPercent: 42,
    windowMinutes: 10080,
    resetsAt: 1789435448,
    creditsHas: true,
    creditsUnlimited: false,
    creditsBalance: "100.0",
    observedAt: "2026-09-09T10:00:00Z",
    ...overrides,
  };
}

function claudeSnapshot(overrides: Partial<ServiceRateLimitWindow> = {}): ServiceRateLimitWindow {
  return {
    vendor: "claude",
    accountId: "acct-1",
    accountLabel: "Acme Org",
    windowKind: "five_hour",
    usedPercent: 33,
    resetsAt: 1789435448,
    observedAt: "2026-09-09T10:00:00Z",
    ...overrides,
  };
}

function codexIdentity(overrides: Partial<RateLimitCardIdentity> = {}): RateLimitCardIdentity {
  return {
    vendor: "codex",
    accountId: "",
    machine: "laptop",
    limitId: "codex",
    windowKind: "primary",
    ...overrides,
  };
}

function claudeIdentity(overrides: Partial<RateLimitCardIdentity> = {}): RateLimitCardIdentity {
  return {
    vendor: "claude",
    accountId: "acct-1",
    machine: "",
    limitId: "",
    windowKind: "five_hour",
    ...overrides,
  };
}

let rateLimits: (typeof import("./ratelimits.svelte.js"))["rateLimits"];

beforeEach(async () => {
  vi.clearAllMocks();
  ({ rateLimits } = await import("./ratelimits.svelte.js"));
  rateLimits.current = [];
  rateLimits.history = {};
  rateLimits.error = null;
  rateLimits.loaded = false;
  rateLimits.refreshToken = 0;
});

describe("rateLimits store", () => {
  it("has no data before the first fetch", () => {
    expect(rateLimits.hasData).toBe(false);
  });

  it("fetchCurrent populates current snapshots and bumps refreshToken", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([codexSnapshot()]);

    await rateLimits.fetchCurrent();

    expect(rateLimits.hasData).toBe(true);
    expect(rateLimits.current).toHaveLength(1);
    expect(rateLimits.current[0]?.limitId).toBe("codex");
    expect(rateLimits.error).toBeNull();
    expect(rateLimits.loaded).toBe(true);
    expect(rateLimits.refreshToken).toBe(1);

    // A second refresh (mirroring the Usage page's manual/periodic
    // refresh calling fetchCurrent again) bumps it again, which
    // RateLimitCard's effect depends on to re-fetch history.
    await rateLimits.fetchCurrent();
    expect(rateLimits.refreshToken).toBe(2);
  });

  it("fetchCurrent records an error and clears loading on failure", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockRejectedValue(new Error("boom"));

    await rateLimits.fetchCurrent();

    expect(rateLimits.error).toBe("boom");
    expect(rateLimits.loading).toBe(false);
    expect(rateLimits.loaded).toBe(true);
    expect(rateLimits.refreshToken).toBe(0);
  });

  // Roborev finding s9js #3: fetchCurrent ignored the shared
  // sessions.filters.agent selection entirely, so a non-Codex agent
  // filter still fetched (and showed) Codex rate-limit cards.
  it("fetchCurrent forwards the shared machine and agent filters", async () => {
    const { sessions } = await import("./sessions.svelte.js");
    sessions.filters.machine = "laptop";
    sessions.filters.agent = "claude";
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([]);

    await rateLimits.fetchCurrent();

    expect(rateLimitsServiceMocks.getApiV1RateLimitsCurrent).toHaveBeenCalledWith(
      { machine: "laptop", agent: "claude" },
      undefined,
    );

    sessions.filters.machine = "";
    sessions.filters.agent = "";
  });

  it("fetchHistory stores results keyed by the full card identity", async () => {
    const history = [
      codexSnapshot({ observedAt: "2026-09-08T10:00:00Z", usedPercent: 10 }),
      codexSnapshot(),
    ];
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue(history);

    await rateLimits.fetchHistory(codexIdentity(), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");

    expect(rateLimits.historyFor(codexIdentity())).toEqual(history);
    expect(rateLimits.historyFor(codexIdentity({ windowKind: "secondary" }))).toEqual([]);
    expect(rateLimitsServiceMocks.getApiV1RateLimitsHistory).toHaveBeenCalledWith(
      expect.objectContaining({
        vendor: "codex",
        machine: "laptop",
        limit_id: "codex",
        window: "primary",
        since: "2026-09-08T00:00:00Z",
        until: "2026-09-09T23:59:59Z",
      }),
      undefined,
    );
  });

  it("fetchHistory keys Claude history by account id rather than limit id", async () => {
    const history = [claudeSnapshot()];
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue(history);

    await rateLimits.fetchHistory(
      claudeIdentity(),
      "2026-09-08T00:00:00Z",
      "2026-09-09T23:59:59Z",
    );

    expect(rateLimits.historyFor(claudeIdentity())).toEqual(history);
    expect(rateLimits.historyFor(claudeIdentity({ accountId: "acct-2" }))).toEqual([]);
    expect(rateLimitsServiceMocks.getApiV1RateLimitsHistory).toHaveBeenCalledWith(
      expect.objectContaining({
        vendor: "claude",
        account_id: "acct-1",
        window: "five_hour",
      }),
      undefined,
    );
  });

  it("fetchHistory leaves prior history in place on failure", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValueOnce([codexSnapshot()]);
    await rateLimits.fetchHistory(codexIdentity(), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");
    expect(rateLimits.historyFor(codexIdentity())).toHaveLength(1);

    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockRejectedValueOnce(new Error("boom"));
    await rateLimits.fetchHistory(codexIdentity(), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");
    expect(rateLimits.historyFor(codexIdentity())).toHaveLength(1);
  });

  // A stale in-flight request for the same identity can still resolve
  // after a newer one already landed (abort does not guarantee the
  // underlying promise never settles); the newer request's result must
  // win regardless of resolution order.
  it("ignores a same-identity response that resolves after a newer request for it", async () => {
    let resolveFirst: ((value: ServiceRateLimitWindow[]) => void) | undefined;
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveFirst = resolve;
        }),
    );
    const firstPromise = rateLimits.fetchHistory(
      codexIdentity(),
      "2026-09-08T00:00:00Z",
      "2026-09-09T23:59:59Z",
    );

    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValueOnce([
      codexSnapshot({ usedPercent: 99 }),
    ]);
    await rateLimits.fetchHistory(codexIdentity(), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");

    resolveFirst?.([codexSnapshot({ usedPercent: 1 })]);
    await firstPromise;

    expect(rateLimits.historyFor(codexIdentity())).toEqual([codexSnapshot({ usedPercent: 99 })]);
  });

  it("groupedByVendor groups snapshots by vendor then account", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsCurrent.mockResolvedValue([
      codexSnapshot(),
      claudeSnapshot(),
      claudeSnapshot({ accountId: "acct-2", accountLabel: "Other Org", windowKind: "seven_day" }),
    ]);

    await rateLimits.fetchCurrent();

    const groups = rateLimits.groupedByVendor;
    expect(groups.map((g) => g.vendor)).toEqual(["codex", "claude"]);
    const claudeGroup = groups.find((g) => g.vendor === "claude");
    expect(claudeGroup?.accounts.map((a) => a.accountId)).toEqual(["acct-1", "acct-2"]);
    expect(claudeGroup?.accounts[0]?.accountLabel).toBe("Acme Org");
    expect(claudeGroup?.accounts[0]?.windows).toHaveLength(1);
  });

  // Roborev finding cyt9 #3: history identity previously omitted machine
  // and plan, so two cards sharing a limit id and window kind (different
  // machine, or the same machine on a different plan) would collide --
  // request cancellation and cache overwrites meant their charts could
  // show the wrong machine's or the wrong plan's history.
  describe("multi-machine isolation and plan-type label handling (roborev cyt9 #3, k5bm)", () => {
    it("keeps history separate for the same limit id and window on two machines", async () => {
      const laptopHistory = [codexSnapshot({ machine: "laptop", usedPercent: 20 })];
      const desktopHistory = [codexSnapshot({ machine: "desktop", usedPercent: 80 })];
      rateLimitsServiceMocks.getApiV1RateLimitsHistory
        .mockResolvedValueOnce(laptopHistory)
        .mockResolvedValueOnce(desktopHistory);

      await rateLimits.fetchHistory(
        codexIdentity({ machine: "laptop" }),
        "2026-09-08T00:00:00Z",
        "2026-09-09T23:59:59Z",
      );
      await rateLimits.fetchHistory(
        codexIdentity({ machine: "desktop" }),
        "2026-09-08T00:00:00Z",
        "2026-09-09T23:59:59Z",
      );

      expect(rateLimits.historyFor(codexIdentity({ machine: "laptop" }))).toEqual(laptopHistory);
      expect(rateLimits.historyFor(codexIdentity({ machine: "desktop" }))).toEqual(desktopHistory);

      // Each machine's own machine (not a shared/global filter) is what
      // gets sent to the server.
      expect(rateLimitsServiceMocks.getApiV1RateLimitsHistory).toHaveBeenNthCalledWith(
        1,
        expect.objectContaining({ machine: "laptop" }),
        undefined,
      );
      expect(rateLimitsServiceMocks.getApiV1RateLimitsHistory).toHaveBeenNthCalledWith(
        2,
        expect.objectContaining({ machine: "desktop" }),
        undefined,
      );
    });

    it("does not fragment one window's history across a plan_type change (roborev k5bm)", async () => {
      // planType is a label, not identity (see RateLimitCardIdentity):
      // a live Codex stream can alternate a limit bucket's plan_type
      // between a real value and null/empty across events for reasons
      // unrelated to which window this is. A single fetchHistory call
      // for one identity must keep every observation regardless of
      // planType, rather than the old behavior of keying and filtering
      // by planType and producing a second, phantom card.
      rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValue([
        codexSnapshot({ planType: "pro", usedPercent: 10 }),
        codexSnapshot({ planType: "", usedPercent: 20 }),
        codexSnapshot({ planType: "pro", usedPercent: 30 }),
      ]);

      await rateLimits.fetchHistory(
        codexIdentity(),
        "2026-09-08T00:00:00Z",
        "2026-09-09T23:59:59Z",
      );

      const rows = rateLimits.historyFor(codexIdentity());
      expect(rows).toHaveLength(3);
      expect(rows.map((row) => row.usedPercent)).toEqual([10, 20, 30]);
    });

    it("does not cancel a same-limit/window request for a different machine", async () => {
      let resolveLaptop: ((value: ServiceRateLimitWindow[]) => void) | undefined;
      rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveLaptop = resolve;
          }),
      );
      const laptopPromise = rateLimits.fetchHistory(
        codexIdentity({ machine: "laptop" }),
        "2026-09-08T00:00:00Z",
        "2026-09-09T23:59:59Z",
      );

      rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValueOnce([
        codexSnapshot({ machine: "desktop" }),
      ]);
      await rateLimits.fetchHistory(
        codexIdentity({ machine: "desktop" }),
        "2026-09-08T00:00:00Z",
        "2026-09-09T23:59:59Z",
      );

      resolveLaptop?.([codexSnapshot({ machine: "laptop" })]);
      await laptopPromise;

      expect(rateLimits.historyFor(codexIdentity({ machine: "laptop" }))).toHaveLength(1);
      expect(rateLimits.historyFor(codexIdentity({ machine: "desktop" }))).toHaveLength(1);
    });
  });

  // Codex reports plan_type as a label that can flip between "pro" and
  // empty for the same window from one observation to the next, not a
  // stable identity component (see RateLimitCardIdentity), so
  // fetchHistory must keep every row for an identity regardless of each
  // row's own plan_type rather than dropping the ones that disagree.
  it("keeps every row for an identity regardless of differing plan_type", async () => {
    rateLimitsServiceMocks.getApiV1RateLimitsHistory.mockResolvedValueOnce([
      codexSnapshot({ planType: "pro" }),
      codexSnapshot({ planType: "" }),
    ]);
    await rateLimits.fetchHistory(codexIdentity(), "2026-09-08T00:00:00Z", "2026-09-09T23:59:59Z");
    expect(rateLimits.historyFor(codexIdentity())).toHaveLength(2);
  });
});
