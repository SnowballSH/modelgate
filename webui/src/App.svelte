<script lang="ts">
  import { ThemeToggle } from "foundationui/svelte";
  import { ApiError, createKey, getUsage, listKeys, revokeKey } from "./lib/api";
  import { toasts } from "./lib/toasts.svelte";
  import type { ApiKey, CreateKeyRequest, UsageResponse } from "./lib/types";
  import CreateKeyForm from "./components/CreateKeyForm.svelte";
  import FullKeyModal from "./components/FullKeyModal.svelte";
  import KeyTable from "./components/KeyTable.svelte";
  import RevokeDialog from "./components/RevokeDialog.svelte";
  import SpendMeter from "./components/SpendMeter.svelte";
  import Toasts from "./components/Toasts.svelte";

  let keys = $state<ApiKey[]>([]);
  let usage = $state<UsageResponse | null>(null);
  let keysLoading = $state(true);
  let usageLoading = $state(true);
  let creating = $state(false);
  let revoking = $state(false);

  let fullKeyOpen = $state(false);
  let fullKey = $state("");
  let fullKeyLabel = $state("");

  let revokeOpen = $state(false);
  let revokeTarget = $state<ApiKey | null>(null);

  function describe(error: unknown): string {
    return error instanceof ApiError
      ? error.message
      : "An unexpected error occurred";
  }

  async function refreshKeys(): Promise<void> {
    try {
      keys = (await listKeys()).keys;
    } catch (error) {
      toasts.push(`Failed to load keys: ${describe(error)}`);
    } finally {
      keysLoading = false;
    }
  }

  async function refreshUsage(): Promise<void> {
    try {
      usage = await getUsage();
    } catch (error) {
      toasts.push(`Failed to load usage: ${describe(error)}`);
    } finally {
      usageLoading = false;
    }
  }

  $effect(() => {
    void refreshKeys();
    void refreshUsage();
  });

  async function handleCreate(body: CreateKeyRequest): Promise<boolean> {
    creating = true;
    try {
      const response = await createKey(body);
      fullKey = response.full_key;
      fullKeyLabel = response.key.label;
      fullKeyOpen = true;
      toasts.push(`Key "${response.key.label}" created`, "success");
      void refreshKeys();
      void refreshUsage();
      return true;
    } catch (error) {
      toasts.push(`Failed to create key: ${describe(error)}`);
      return false;
    } finally {
      creating = false;
    }
  }

  function requestRevoke(key: ApiKey): void {
    revokeTarget = key;
    revokeOpen = true;
  }

  async function confirmRevoke(): Promise<void> {
    if (!revokeTarget) return;
    revoking = true;
    try {
      const response = await revokeKey(revokeTarget.id);
      keys = keys.map((key) => (key.id === response.key.id ? response.key : key));
      toasts.push(`Key "${response.key.label}" revoked`, "success");
      revokeOpen = false;
    } catch (error) {
      toasts.push(`Failed to revoke key: ${describe(error)}`);
    } finally {
      revoking = false;
    }
  }
</script>

<div class="min-h-screen bg-surface text-ink">
  <header class="border-b border-line">
    <div
      class="mx-auto flex max-w-6xl items-center justify-between gap-4 px-4 py-4"
    >
      <h1 class="text-xl font-semibold">Model Gateway Admin</h1>
      <ThemeToggle />
    </div>
  </header>
  <main class="mx-auto flex max-w-6xl flex-col gap-6 px-4 py-6">
    <SpendMeter {usage} loading={usageLoading} />
    <CreateKeyForm submitting={creating} oncreate={handleCreate} />
    <section aria-label="API keys" class="flex flex-col gap-3">
      <h2 class="text-lg font-semibold">Keys</h2>
      <KeyTable {keys} loading={keysLoading} onrevoke={requestRevoke} />
    </section>
  </main>
</div>

<FullKeyModal bind:open={fullKeyOpen} {fullKey} label={fullKeyLabel} />
<RevokeDialog
  bind:open={revokeOpen}
  key={revokeTarget}
  {revoking}
  onconfirm={confirmRevoke}
/>
<Toasts />
