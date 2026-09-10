import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";

import { withCrossTabLock } from "./crossTabLock.js";

let originalLocks: unknown;

function setWebLocks(locks: unknown): void {
  Object.defineProperty(navigator, "locks", {
    value: locks,
    configurable: true,
    writable: true,
  });
}

beforeEach(() => {
  originalLocks = (navigator as { locks?: unknown }).locks;
});

afterEach(() => {
  setWebLocks(originalLocks);
  vi.restoreAllMocks();
});

describe("withCrossTabLock", () => {
  it("with the Web Locks API present, serializes fn through navigator.locks.request under a fixed lock name", async () => {
    const request = vi.fn(async (_name: string, callback: () => unknown) => callback());
    setWebLocks({ request });

    const result = await withCrossTabLock(() => "done");

    expect(result).toBe("done");
    expect(request).toHaveBeenCalledOnce();
    expect(request.mock.calls[0]?.[0]).toBe("agentsview-rate-limit-alert-check");
  });

  it("with the Web Locks API absent, still runs and delivers fn's result, uncoordinated", async () => {
    setWebLocks(undefined);
    vi.spyOn(console, "warn").mockImplementation(() => {});

    const result = await withCrossTabLock(() => "delivered");

    expect(result).toBe("delivered");
  });
});
