<script lang="ts">
  import { Badge } from "foundationui/svelte";
  import type { KeyState } from "../lib/types";

  let { state }: { state: KeyState } = $props();

  const tone = $derived(
    state === "active" ? "aurora" : state === "expired" ? "accent" : "neutral",
  );
</script>

<Badge {tone}>{state}</Badge>
