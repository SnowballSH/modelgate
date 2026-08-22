<script lang="ts">
  import { formatUsd } from "../lib/format";

  let {
    label,
    spend,
    limit,
  }: { label: string; spend: number; limit: number } = $props();

  const ratio = $derived(limit > 0 ? Math.min(spend / limit, 1) : 0);
  const warning = $derived(ratio >= 0.8);
</script>

<div
  role="progressbar"
  aria-label={label}
  aria-valuemin={0}
  aria-valuemax={limit}
  aria-valuenow={Math.min(spend, limit)}
  aria-valuetext={`${formatUsd(spend)} of ${formatUsd(limit)}`}
  class="h-2 w-full overflow-hidden rounded-full bg-glass-3"
>
  <div
    class={`h-full rounded-full transition-[width] duration-300 motion-reduce:transition-none ${
      warning ? "bg-amber-500" : "bg-accent"
    }`}
    style={`width: ${(ratio * 100).toFixed(1)}%`}
  ></div>
</div>
