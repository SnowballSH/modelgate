<script lang="ts">
  import { Button, Input, Panel, Spinner } from "foundationui/svelte";
  import type { CreateKeyRequest } from "../lib/types";

  let {
    submitting,
    oncreate,
  }: {
    submitting: boolean;
    oncreate: (body: CreateKeyRequest) => Promise<boolean>;
  } = $props();

  let label = $state("");
  let models = $state("");
  let quota = $state("");
  let expires = $state("");
  let labelInvalid = $state(false);

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    const trimmedLabel = label.trim();
    labelInvalid = trimmedLabel.length === 0;
    if (labelInvalid) return;

    const body: CreateKeyRequest = { label: trimmedLabel };
    const modelList = models
      .split(",")
      .map((model) => model.trim())
      .filter((model) => model.length > 0);
    if (modelList.length > 0) body.models = modelList;
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
      models = "";
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
        <label for="create-models" class="text-sm font-medium text-ink">
          Allowed models
        </label>
        <Input
          id="create-models"
          name="models"
          placeholder="gpt-5.2, claude-fable-5"
          aria-describedby="create-models-hint"
          bind:value={models}
        />
        <p id="create-models-hint" class="text-xs text-ink-secondary">
          Comma-separated. Empty allows all models.
        </p>
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
