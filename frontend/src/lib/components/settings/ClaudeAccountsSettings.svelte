<script lang="ts">
  import { onMount } from "svelte";
  import { Button, EmptyState } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import { claudeAccounts } from "../../stores/claudeAccounts.svelte.js";
  import { formatRelativeTime } from "../../utils/format.js";

  onMount(() => {
    claudeAccounts.fetch();
  });
</script>

<div class="claude-accounts-settings">
  <p class="hint">{m.settings_claude_accounts_config_hint()}</p>

  {#if claudeAccounts.error}
    <p class="msg error">{claudeAccounts.error}</p>
  {:else if claudeAccounts.loaded && claudeAccounts.accounts.length === 0}
    <EmptyState
      title={m.settings_claude_accounts_empty_title()}
      description={m.settings_claude_accounts_empty_description()}
    />
  {:else}
    <ul class="account-list">
      {#each claudeAccounts.accounts as account (account.name)}
        {@const testResult = claudeAccounts.testResult[account.name]}
        <li class="account-row">
          <div class="account-identity">
            <span class="account-name">{account.name}</span>
            {#if account.identityAvailable}
              <span class="account-label">{account.label}</span>
              {#if account.userRateLimitTier}
                <span class="account-tier">{account.userRateLimitTier}</span>
              {/if}
            {:else}
              <span class="account-no-identity">
                {account.identityError || m.settings_claude_accounts_no_identity()}
              </span>
            {/if}
          </div>

          <div class="account-status">
            <span class="status-item">
              <span class="status-label">{m.settings_claude_accounts_last_poll()}</span>
              <span class="status-value">{formatRelativeTime(account.lastPoll)}</span>
            </span>
            {#if account.lastError}
              <span class="status-item">
                <span class="status-label">{m.settings_claude_accounts_last_error()}</span>
                <span class="status-value error" title={account.lastError}
                  >{account.lastError}</span
                >
              </span>
            {/if}
            {#if testResult && !testResult.success}
              <span class="status-item">
                <span class="status-value error">
                  {testResult.error || m.settings_claude_accounts_test_failed()}
                </span>
              </span>
            {/if}
          </div>

          <Button
            size="sm"
            disabled={claudeAccounts.testing[account.name]}
            onclick={() => claudeAccounts.test(account.name)}
          >
            {claudeAccounts.testing[account.name]
              ? m.settings_claude_accounts_testing()
              : m.settings_claude_accounts_test()}
          </Button>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .claude-accounts-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-4, 12px);
  }

  .hint {
    font-size: 11px;
    color: var(--text-muted);
    margin: 0;
  }

  .account-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .account-row {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 10px 12px;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md, 8px);
    background: var(--bg-surface);
  }

  .account-identity {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 140px;
  }

  .account-name {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
    font-family: var(--font-mono, monospace);
  }

  .account-label {
    font-size: 11px;
    color: var(--text-secondary);
  }

  .account-tier {
    font-size: 10px;
    color: var(--text-muted);
  }

  .account-no-identity {
    font-size: 11px;
    color: var(--accent-amber, #f59e0b);
  }

  .account-status {
    display: flex;
    flex-direction: column;
    gap: 2px;
    flex: 1;
    min-width: 0;
  }

  .status-item {
    display: flex;
    gap: 6px;
    font-size: 11px;
  }

  .status-label {
    color: var(--text-muted);
  }

  .status-value {
    color: var(--text-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .status-value.error {
    color: var(--accent-red, #ef4444);
  }

  .msg.error {
    font-size: 11px;
    color: var(--accent-red, #ef4444);
    margin: 0;
  }
</style>
