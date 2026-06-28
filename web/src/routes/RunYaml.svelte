<script>
    import { onMount } from "svelte";
    import AuthSetup from "../components/AuthSetup.svelte";
    import { apiFetchText } from "../lib/api.js";

    export let params;
    $: runID = params.id;

    let yamlText = "",
        loading = true,
        error = "";

    async function load() {
        loading = true;
        error = "";
        yamlText = "";
        try {
            yamlText = await apiFetchText(
                "/api/v1/runs/" + encodeURIComponent(runID) + "/yaml",
            );
        } catch (e) {
            error = e.message;
        } finally {
            loading = false;
        }
    }
    onMount(load);
    $: runID, load();
</script>

<div class="container">
    <AuthSetup />
    <div style="display:flex;align-items:center;gap:1rem;margin-bottom:1rem">
        <a href="#/runs/{runID}">← Run Detail</a>
        <h1>Run YAML</h1>
    </div>
    <div style="border-bottom:1px solid var(--border);margin-bottom:1.5rem">
        <a href="#/runs/{runID}" class="tab-link">Logs</a>
        <a href="#/runs/{runID}/yaml" class="tab-link tab-active">YAML</a>
    </div>
    {#if loading}<div class="loading">Loading...</div>
    {:else if error}<div class="error">{error}</div>
    {:else}
        <pre
            class="log-box"
            style="height:auto;min-height:60vh">{yamlText}</pre>
    {/if}
</div>
