<script lang="ts">
  import { toasts } from "../lib/toasts.svelte";
</script>

<div
  class="fixed right-4 bottom-4 z-50 flex w-80 max-w-[calc(100vw-2rem)] flex-col gap-2"
  role="region"
  aria-label="Notifications"
  aria-live="polite"
>
  {#each toasts.items as toast (toast.id)}
    <div
      role={toast.tone === "error" ? "alert" : "status"}
      class={`flex items-start justify-between gap-3 rounded-lg border p-3 text-sm shadow-float-1 ${
        toast.tone === "error"
          ? "border-red-500/50 bg-surface-raised text-ink"
          : "border-aurora/40 bg-surface-raised text-ink"
      }`}
    >
      <p>{toast.message}</p>
      <button
        type="button"
        class="shrink-0 rounded text-ink-secondary hover:text-ink focus-visible:outline-2 focus-visible:outline-accent"
        aria-label="Dismiss notification"
        onclick={() => toasts.dismiss(toast.id)}
      >
        &times;
      </button>
    </div>
  {/each}
</div>
