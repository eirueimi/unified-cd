<script>
    import { onMount } from "svelte";
    import AuthSetup from "../components/AuthSetup.svelte";
    import { apiFetchText } from "../lib/api.js";

    export let params;
    $: jobName = params.name;

    let yamlText = "",
        loading = true,
        error = "";

    async function load() {
        loading = true;
        error = "";
        yamlText = "";
        try {
            yamlText = await apiFetchText(
                "/api/v1/jobs/" + encodeURIComponent(jobName) + "/yaml",
            );
        } catch (e) {
            error = e.message;
        } finally {
            loading = false;
        }
    }
    onMount(load);
    $: jobName, load();
</script>

<div class="container">
    <AuthSetup />
    <div style="display:flex;align-items:center;gap:1rem;margin-bottom:1rem">
        <a href="#/">← Jobs</a>
        <h1>{jobName}</h1>
    </div>
    <div style="border-bottom:1px solid var(--border);margin-bottom:1.5rem">
        <a href="#/jobs/{jobName}" class="tab-link">History</a>
        <a href="#/jobs/{jobName}/run" class="tab-link">▶ Run</a>
        <a href="#/jobs/{jobName}/yaml" class="tab-link tab-active">YAML</a>
    </div>
    {#if loading}<div class="loading">Loading...</div>
    {:else if error}<div class="error">{error}</div>
    {:else}
        <pre
            class="log-box"
            style="height:auto;min-height:60vh">{yamlText}</pre>
    {/if}
</div>
