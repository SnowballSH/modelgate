<script lang="ts">
  import { Panel, Skeleton } from "foundationui/svelte";
  import { formatMonth, formatUsd } from "../lib/format";
  import type { UsageResponse } from "../lib/types";
  import SpendBar from "./SpendBar.svelte";

  let { usage, loading }: { usage: UsageResponse | null; loading: boolean } =
    $props();

  const quotaKeys = $derived(
    usage?.keys.filter(
      (key): key is typeof key & { quota_usd: number } =>
        key.quota_usd !== null && key.quota_usd > 0,
    ) ?? [],
  );
</script>

<Panel tier="raised" padding="lg" class="flex flex-col gap-4">
  <h2 class="text-lg font-semibold text-ink">Monthly spend</h2>
  {#if loading}
    <Skeleton class="h-6 w-1/2" />
    <Skeleton class="h-2 w-full" />
  {:else if usage}
    <div class="flex items-baseline justify-between gap-4">
      <p class="text-sm text-ink-secondary">{formatMonth(usage.month)}</p>
      <p class="font-mono text-sm text-ink">
        {formatUsd(usage.spend_usd)}
        <span class="text-ink-secondary">/ {formatUsd(usage.budget_usd)}</span>
      </p>
    </div>
    <SpendBar
      label="Month-to-date spend against budget"
      spend={usage.spend_usd}
      limit={usage.budget_usd}
    />
    {#if quotaKeys.length > 0}
      <div class="flex flex-col gap-3 border-t border-line pt-4">
        <h3 class="text-sm font-medium text-ink-secondary">Per-key quotas</h3>
        {#each quotaKeys as key (key.id)}
          <div class="flex flex-col gap-1">
            <div class="flex items-baseline justify-between gap-4 text-sm">
              <span class="truncate text-ink">{key.label}</span>
              <span class="shrink-0 font-mono text-xs text-ink-secondary">
                {formatUsd(key.spend_usd)} / {formatUsd(key.quota_usd)}
              </span>
            </div>
            <SpendBar
              label={`Spend for key ${key.label}`}
              spend={key.spend_usd}
              limit={key.quota_usd}
            />
          </div>
        {/each}
      </div>
    {/if}
  {:else}
    <p class="text-sm text-ink-secondary">Usage data is unavailable.</p>
  {/if}
</Panel>
