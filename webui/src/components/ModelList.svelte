<script lang="ts">
  import { Badge, Panel, Skeleton } from "foundationui/svelte";
  import type { ConfiguredModel } from "../lib/types";

  let {
    models,
    loading,
  }: { models: ConfiguredModel[] | null; loading: boolean } = $props();
</script>

<Panel tier="raised" padding="lg" class="flex flex-col gap-3">
  <h2 class="text-lg font-semibold text-ink">Models</h2>
  {#if loading}
    <Skeleton class="h-5 w-2/3" />
  {:else if models === null}
    <p class="text-sm text-ink-secondary">The model list is unavailable.</p>
  {:else if models.length === 0}
    <p class="text-sm text-ink-secondary">No models are configured.</p>
  {:else}
    <ul class="flex flex-wrap gap-2" aria-label="Configured models">
      {#each models as model (model.id)}
        <li>
          <Badge tone="neutral" class="font-mono text-xs">
            <span class="text-ink">{model.id}</span>
            <span class="font-sans text-[10px] tracking-wide uppercase">
              {model.provider}
            </span>
          </Badge>
        </li>
      {/each}
    </ul>
  {/if}
</Panel>
