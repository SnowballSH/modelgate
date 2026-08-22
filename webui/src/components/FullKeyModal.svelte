<script lang="ts">
  import { Button, Callout, Dialog } from "foundationui/svelte";
  import { toasts } from "../lib/toasts.svelte";

  let {
    open = $bindable(false),
    fullKey,
    label,
  }: { open?: boolean; fullKey: string; label: string } = $props();

  let copied = $state(false);

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(fullKey);
      copied = true;
    } catch {
      toasts.push("Could not access the clipboard. Copy the key manually.");
    }
  }

  function close(): void {
    copied = false;
  }
</script>

<Dialog
  bind:open
  title="API key created"
  description={`The key "${label}" was created. Store it now.`}
  onclose={close}
>
  <div class="flex flex-col gap-4">
    <Callout tone="warn">
      This is the only time the full key is shown. It cannot be retrieved
      again.
    </Callout>
    <output
      class="block rounded-lg border border-line bg-glass-1 p-3 font-mono text-sm break-all text-ink select-all"
      aria-label="Full API key"
    >
      {fullKey}
    </output>
  </div>
  {#snippet footer()}
    <Button variant="secondary" onclick={copy}>
      {copied ? "Copied" : "Copy key"}
    </Button>
    <Button onclick={() => (open = false)}>Done</Button>
  {/snippet}
</Dialog>
