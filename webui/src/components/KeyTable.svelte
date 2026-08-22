<script lang="ts">
  import { Badge, Button, Panel, Skeleton } from "foundationui/svelte";
  import { formatTimestamp, formatUsd } from "../lib/format";
  import { keyState, type ApiKey } from "../lib/types";
  import StateBadge from "./StateBadge.svelte";

  let {
    keys,
    loading,
    onrevoke,
  }: {
    keys: ApiKey[];
    loading: boolean;
    onrevoke: (key: ApiKey) => void;
  } = $props();
</script>

<Panel tier="raised" padding="none" class="overflow-x-auto">
  <table class="w-full min-w-[56rem] text-left text-sm">
    <caption class="sr-only">API keys</caption>
    <thead>
      <tr class="border-b border-line text-xs text-ink-secondary uppercase">
        <th scope="col" class="px-4 py-3 font-medium">Key</th>
        <th scope="col" class="px-4 py-3 font-medium">Label</th>
        <th scope="col" class="px-4 py-3 font-medium">Models</th>
        <th scope="col" class="px-4 py-3 font-medium">Spend / quota</th>
        <th scope="col" class="px-4 py-3 font-medium">Last used</th>
        <th scope="col" class="px-4 py-3 font-medium">State</th>
        <th scope="col" class="px-4 py-3 font-medium">Created</th>
        <th scope="col" class="px-4 py-3 font-medium">
          <span class="sr-only">Actions</span>
        </th>
      </tr>
    </thead>
    <tbody>
      {#if loading}
        {#each { length: 3 } as _, row (row)}
          <tr class="border-b border-line last:border-b-0">
            {#each { length: 8 } as _, cell (cell)}
              <td class="px-4 py-3"><Skeleton class="h-4 w-full" /></td>
            {/each}
          </tr>
        {/each}
      {:else if keys.length === 0}
        <tr>
          <td colspan="8" class="px-4 py-8 text-center text-ink-secondary">
            No API keys yet. Create one above.
          </td>
        </tr>
      {:else}
        {#each keys as key (key.id)}
          {@const state = keyState(key)}
          <tr class="border-b border-line last:border-b-0">
            <td class="px-4 py-3 font-mono text-xs text-ink">
              {key.prefix}&hellip;
            </td>
            <td class="px-4 py-3 text-ink">{key.label}</td>
            <td class="px-4 py-3">
              {#if key.models === null || key.models.length === 0}
                <span class="text-ink-secondary">all</span>
              {:else}
                <span class="flex flex-wrap gap-1">
                  {#each key.models as model (model)}
                    <Badge tone="neutral" class="font-mono text-xs">
                      {model}
                    </Badge>
                  {/each}
                </span>
              {/if}
            </td>
            <td class="px-4 py-3 font-mono text-xs whitespace-nowrap">
              <span class="text-ink">{formatUsd(key.month_spend_usd)}</span>
              <span class="text-ink-secondary">
                / {key.quota_usd === null ? "no quota" : formatUsd(key.quota_usd)}
              </span>
            </td>
            <td class="px-4 py-3 whitespace-nowrap text-ink-secondary">
              {formatTimestamp(key.last_used_at)}
            </td>
            <td class="px-4 py-3"><StateBadge {state} /></td>
            <td class="px-4 py-3 text-ink-secondary">
              <span class="block whitespace-nowrap">{key.created_by}</span>
              <span class="block text-xs whitespace-nowrap">
                {formatTimestamp(key.created_at)}
              </span>
            </td>
            <td class="px-4 py-3 text-right">
              {#if state === "active"}
                <Button
                  variant="ghost"
                  size="sm"
                  onclick={() => onrevoke(key)}
                >
                  Revoke
                </Button>
              {/if}
            </td>
          </tr>
        {/each}
      {/if}
    </tbody>
  </table>
</Panel>
