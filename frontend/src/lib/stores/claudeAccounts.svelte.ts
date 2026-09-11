import { ClaudeService, type ClaudeAccountInfo } from "../api/generated/index";
import { callGenerated, isAbortError } from "../api/runtime.js";

export type { ClaudeAccountInfo };

/**
 * Configured `[claude.accounts.<name>]` entries for the read-only "Claude
 * accounts" settings panel: identity, tier, and background-poller status.
 * Credentials themselves are never fetched or displayed -- the panel is
 * read-only for secrets, which are configured in config.toml only.
 */
class ClaudeAccountsStore {
  accounts: ClaudeAccountInfo[] = $state([]);
  loading = $state(false);
  loaded = $state(false);
  error: string | null = $state(null);

  /** Account names with an in-flight "Test" trigger. */
  testing: Record<string, boolean> = $state({});
  testResult: Record<string, { success: boolean; error?: string }> = $state({});

  private abort?: AbortController;

  async fetch(): Promise<void> {
    this.abort?.abort();
    const controller = new AbortController();
    this.abort = controller;
    this.loading = true;
    try {
      const data = await callGenerated(
        (options) => ClaudeService.getApiV1ClaudeAccounts(options),
        controller.signal,
      );
      this.accounts = data.accounts;
      this.error = null;
      this.loaded = true;
    } catch (err) {
      if (isAbortError(err)) return;
      this.error = err instanceof Error ? err.message : String(err);
      this.loaded = true;
    } finally {
      this.loading = false;
    }
  }

  async test(name: string): Promise<void> {
    if (this.testing[name]) return;
    this.testing = { ...this.testing, [name]: true };
    try {
      const result = await ClaudeService.postApiV1ClaudeAccountsByNameTest({ name });
      this.testResult = { ...this.testResult, [name]: result };
      if (result.success) {
        // A successful trigger runs the poll synchronously, so the
        // account's status (last poll / last error) is already fresh.
        await this.fetch();
      }
    } catch (err) {
      this.testResult = {
        ...this.testResult,
        [name]: { success: false, error: err instanceof Error ? err.message : String(err) },
      };
    } finally {
      this.testing = { ...this.testing, [name]: false };
    }
  }
}

export const claudeAccounts = new ClaudeAccountsStore();
