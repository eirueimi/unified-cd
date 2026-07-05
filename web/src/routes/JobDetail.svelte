<script>
  import { onMount } from "svelte";
  import AuthSetup from "../components/AuthSetup.svelte";
  import { apiFetch } from "../lib/api.js";
  import { statusBadge, fmtRelative } from "../lib/utils.js";

  export let params;
  $: jobName = decodeURIComponent(params.name);

  let runs = [],
    loading = true,
    error = "";

  $: hasParams = runs.some(
    (r) => r.params && Object.keys(r.params).length > 0,
  );

  function fmtParams(params) {
    if (!params) return "";
    return Object.entries(params)
      .map(([k, v]) => `${k}=${v}`)
      .join("  ");
  }

  async function load() {
    loading = true;
    error = "";
    try {
      runs = await apiFetch(
        "/api/v1/runs?jobName=" + encodeURIComponent(jobName),
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
    <a href="#/jobs/{encodeURIComponent(jobName)}" class="tab-link tab-active">History</a>
    <a href="#/jobs/{encodeURIComponent(jobName)}/run" class="tab-link">▶ Run</a>
    <a href="#/jobs/{encodeURIComponent(jobName)}/yaml" class="tab-link">YAML</a>
  </div>
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !runs.length}<div class="empty">No runs yet.</div>
  {:else}
    <table>
      <thead>
        <tr>
          <th>Run ID</th>
          <th>Status</th>
          <th>Triggered by</th>
          <th>Created</th>
          {#if hasParams}<th>Params</th>{/if}
          <th></th>
        </tr>
      </thead>
      <tbody>
        {#each runs as r (r.id)}
          <tr>
            <td><a href="#/runs/{r.id}">{r.id.slice(0, 8)}…</a></td>
            <td><span class={statusBadge(r.status)}>{r.status}</span></td>
            <td class="meta">{r.triggeredBy}</td>
            <td class="meta">{fmtRelative(r.createdAt)}</td>
            {#if hasParams}
              <td class="params-cell">
                {#if r.params && Object.keys(r.params).length}
                  {#each Object.entries(r.params) as [k, v]}
                    <span class="param-tag"
                      ><span class="param-key">{k}</span><span class="param-sep">=</span><span class="param-val">{v}</span></span
                    >
                  {/each}
                {:else}
                  <span class="meta">—</span>
                {/if}
              </td>
            {/if}
            <td><a href="#/runs/{r.id}" class="btn">Logs →</a></td>
          </tr>
        {/each}
      </tbody>
    </table>

  {/if}
</div>

<style>
  .params-cell {
    display: flex;
    flex-wrap: wrap;
    gap: 0.3rem;
    align-items: center;
    max-width: 360px;
  }
  .param-tag {
    display: inline-flex;
    align-items: center;
    background: var(--surface2, rgba(128, 128, 128, 0.12));
    border-radius: 4px;
    font-size: 0.75rem;
    font-family: monospace;
    padding: 0.1rem 0.35rem;
    white-space: nowrap;
    max-width: 200px;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .param-key {
    color: var(--text-muted);
  }
  .param-sep {
    color: var(--text-muted);
    margin: 0 0.1rem;
  }
  .param-val {
    color: var(--text);
    font-weight: 500;
  }
</style>
