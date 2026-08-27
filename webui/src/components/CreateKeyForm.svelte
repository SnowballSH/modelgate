<script lang="ts">
  import { Button, Input, Panel, Spinner, Switch } from "foundationui/svelte";
  import type { ConfiguredModel, CreateKeyRequest } from "../lib/types";

  let {
    submitting,
    models,
    modelsLoading,
    oncreate,
  }: {
    submitting: boolean;
    models: ConfiguredModel[] | null;
    modelsLoading: boolean;
    oncreate: (body: CreateKeyRequest) => Promise<boolean>;
  } = $props();

  let label = $state("");
  let allModels = $state(true);
  let selectedModels = $state<string[]>([]);
  let quota = $state("");
  let expires = $state("");
  let labelInvalid = $state(false);
  let modelsInvalid = $state(false);

  function toggleModel(id: string, selected: boolean): void {
    modelsInvalid = false;
    selectedModels = selected
      ? [...selectedModels, id]
      : selectedModels.filter((model) => model !== id);
  }

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    const trimmedLabel = label.trim();
    labelInvalid = trimmedLabel.length === 0;
    modelsInvalid = !allModels && selectedModels.length === 0;
    if (labelInvalid || modelsInvalid) return;

    const body: CreateKeyRequest = { label: trimmedLabel };
    if (!allModels) body.models = selectedModels;
    const quotaValue = Number.parseFloat(quota);
    if (quota.trim() !== "" && Number.isFinite(quotaValue) && quotaValue > 0) {
      body.quota_usd = quotaValue;
    }
    if (expires !== "") {
      body.expires_at = new Date(`${expires}T23:59:59`).toISOString();
    }

    const created = await oncreate(body);
    if (created) {
      label = "";
      allModels = true;
      selectedModels = [];
      quota = "";
      expires = "";
    }
  }
</script>

<Panel tier="raised" padding="lg">
  <form class="flex flex-col gap-4" onsubmit={submit}>
    <h2 class="text-lg font-semibold text-ink">Create key</h2>
    <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <div class="flex flex-col gap-1">
        <label for="create-label" class="text-sm font-medium text-ink">
          Label <span aria-hidden="true" class="text-red-500">*</span>
        </label>
        <Input
          id="create-label"
          name="label"
          required
          invalid={labelInvalid}
          aria-describedby={labelInvalid ? "create-label-error" : undefined}
          placeholder="team-ci"
          bind:value={label}
          oninput={() => (labelInvalid = false)}
        />
        {#if labelInvalid}
          <p id="create-label-error" class="text-xs text-red-500" role="alert">
            A label is required.
          </p>
        {/if}
      </div>
      <div class="flex flex-col gap-1">
        <span id="create-models-label" class="text-sm font-medium text-ink">
          Allowed models
        </span>
        <div class="flex h-9 items-center gap-2">
          <Switch
            id="create-all-models"
            bind:checked={allModels}
            onCheckedChange={() => (modelsInvalid = false)}
            aria-describedby="create-models-hint"
          />
          <label for="create-all-models" class="text-sm text-ink">
            All models
          </label>
        </div>
        <p id="create-models-hint" class="text-xs text-ink-secondary">
          {allModels
            ? "The key may use every configured model."
            : "Pick the models this key may use."}
        </p>
        {#if !allModels}
          <fieldset
            class="flex max-h-40 flex-col gap-1 overflow-y-auto rounded-sm border border-line bg-glass-3 p-2"
            aria-describedby={modelsInvalid ? "create-models-error" : undefined}
          >
            <legend class="sr-only">Models this key may use</legend>
            {#if modelsLoading}
              <p class="text-xs text-ink-secondary">Loading models&hellip;</p>
            {:else if models === null}
              <p class="text-xs text-ink-secondary">
                The model list could not be loaded.
              </p>
            {:else if models.length === 0}
              <p class="text-xs text-ink-secondary">No models are configured.</p>
            {:else}
              {#each models as model (model.id)}
                <label
                  class="flex cursor-pointer items-center gap-2 rounded-xs px-1 py-0.5 text-sm text-ink hover:bg-glass-2"
                >
                  <input
                    type="checkbox"
                    name="models"
                    value={model.id}
                    checked={selectedModels.includes(model.id)}
                    onchange={(event) =>
                      toggleModel(model.id, event.currentTarget.checked)}
                    class="size-4 shrink-0 accent-accent"
                  />
                  <span class="truncate font-mono text-xs">{model.id}</span>
                  <span
                    class="ml-auto shrink-0 rounded-xs border border-line bg-surface px-1.5 py-px text-[10px] tracking-wide text-ink-secondary uppercase"
                  >
                    {model.provider}
                  </span>
                </label>
              {/each}
            {/if}
          </fieldset>
        {/if}
        {#if modelsInvalid}
          <p id="create-models-error" class="text-xs text-red-500" role="alert">
            Select at least one model, or allow all models.
          </p>
        {/if}
      </div>
      <div class="flex flex-col gap-1">
        <label for="create-quota" class="text-sm font-medium text-ink">
          Monthly quota (USD)
        </label>
        <Input
          id="create-quota"
          name="quota_usd"
          type="number"
          min="0.01"
          step="0.01"
          inputmode="decimal"
          placeholder="10.00"
          bind:value={quota}
        />
      </div>
      <div class="flex flex-col gap-1">
        <label for="create-expires" class="text-sm font-medium text-ink">
          Expires
        </label>
        <Input
          id="create-expires"
          name="expires_at"
          type="date"
          bind:value={expires}
        />
      </div>
    </div>
    <div class="flex items-center gap-3">
      <Button type="submit" disabled={submitting}>Create key</Button>
      {#if submitting}
        <Spinner size="sm" label="Creating key" />
      {/if}
    </div>
  </form>
</Panel>
