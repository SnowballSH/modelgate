<script lang="ts">
  import { Button, Dialog, Spinner } from "foundationui/svelte";
  import type { ApiKey } from "../lib/types";

  let {
    open = $bindable(false),
    key,
    revoking,
    onconfirm,
  }: {
    open?: boolean;
    key: ApiKey | null;
    revoking: boolean;
    onconfirm: () => void;
  } = $props();
</script>

<Dialog
  bind:open
  size="sm"
  title="Revoke key?"
  description={key
    ? `Requests using "${key.label}" (${key.prefix}…) will be rejected immediately. This cannot be undone.`
    : ""}
>
  {#snippet footer()}
    <Button
      variant="secondary"
      disabled={revoking}
      onclick={() => (open = false)}
    >
      Cancel
    </Button>
    <Button disabled={revoking} onclick={onconfirm}>
      {#if revoking}
        <Spinner size="sm" label="Revoking" />
      {:else}
        Revoke
      {/if}
    </Button>
  {/snippet}
</Dialog>
