<script>
  import { onMount } from "svelte";
  import { push } from "svelte-spa-router";
  import AuthSetup from "../components/AuthSetup.svelte";
  import { apiFetch } from "../lib/api.js";

  export let params;
  $: jobName = params.name;

  let inputs = [],
    formParams = {},
    loading = true,
    submitting = false,
    error = "";

  async function load() {
    loading = true;
    error = "";
    try {
      const job = await apiFetch("/api/v1/jobs/" + encodeURIComponent(jobName));
      inputs = job.inputs || [];
      const p = {};
      for (const inp of inputs) {
        if (inp.type === "bool") {
          p[inp.name] =
            inp.default === true || inp.default === "true" ? "true" : "false";
        } else {
          p[inp.name] = inp.default != null ? String(inp.default) : "";
        }
      }
      formParams = p;
    } catch (e) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  $: canRun =
    !submitting &&
    inputs.every(
      (inp) =>
        !inp.required ||
        (formParams[inp.name] !== "" && formParams[inp.name] !== undefined),
    );

  async function triggerRun() {
    submitting = true;
    error = "";
    try {
      const p = {};
      for (const inp of inputs) p[inp.name] = formParams[inp.name] ?? "";
      const run = await apiFetch("/api/v1/runs", {
        method: "POST",
        body: JSON.stringify({ jobName, params: p }),
      });
      push("/runs/" + run.id);
    } catch (e) {
      error = e.message;
    } finally {
      submitting = false;
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
    <a href="#/jobs/{jobName}/run" class="tab-link tab-active">▶ Run</a>
    <a href="#/jobs/{jobName}/yaml" class="tab-link">YAML</a>
  </div>
  {#if loading}<div class="loading">Loading...</div>
  {:else}
    {#if error}<div class="error">{error}</div>{/if}
    <div style="max-width:480px">
      {#if !inputs.length}<p class="meta" style="margin-bottom:1rem">
          このジョブにはパラメータがありません。
        </p>{/if}
      {#each inputs as inp (inp.name)}
        <div style="margin-bottom:1rem">
          <label
            style="display:block;margin-bottom:0.25rem;font-size:0.85rem;color:var(--text)"
          >
            {inp.name}{#if inp.required}<span
                style="color:var(--danger);margin-left:2px">*</span
              >{/if}
          </label>
          {#if inp.description}<div class="form-hint">
              {inp.description}
            </div>{/if}
          {#if inp.type === "string"}
            <input
              type="text"
              class="token-input"
              style="width:100%"
              bind:value={formParams[inp.name]}
            />
          {:else if inp.type === "int"}
            <input
              type="number"
              class="token-input"
              style="width:160px"
              bind:value={formParams[inp.name]}
            />
          {:else if inp.type === "bool"}
            <label
              style="display:flex;align-items:center;gap:0.5rem;cursor:pointer;margin-top:0.25rem"
            >
              <input
                type="checkbox"
                checked={formParams[inp.name] === "true"}
                on:change={(e) =>
                  (formParams[inp.name] = e.target.checked ? "true" : "false")}
              />
              <span class="meta"
                >{formParams[inp.name] === "true" ? "true" : "false"}</span
              >
            </label>
          {/if}
        </div>
      {/each}
      <button
        class="btn"
        disabled={!canRun}
        on:click={triggerRun}
        style="margin-top:0.5rem;padding:0.5rem 1.5rem"
      >
        ▶ {submitting ? "Running..." : "Run"}
      </button>
    </div>
  {/if}
</div>
