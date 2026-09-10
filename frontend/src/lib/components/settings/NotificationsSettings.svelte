<script lang="ts">
  import { Card } from "@kenn-io/kit-ui";
  import { m } from "../../i18n/index.js";
  import RateLimitAlertSettings from "./RateLimitAlertSettings.svelte";
</script>

<!--
  The Notifications panel is a list of cards, one per notification kind.
  Today there is exactly one: rate-limit alerts. A future notification
  kind (e.g. sync failures) is added as another `<Card>` here rather
  than as a separate settings panel.
-->
<div class="notifications-settings">
  <Card title={m.settings_notifications_rate_limits_card_title()}>
    <RateLimitAlertSettings />
  </Card>
</div>

<style>
  .notifications-settings {
    display: flex;
    flex-direction: column;
    gap: var(--space-5);
  }
</style>
