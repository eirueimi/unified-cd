<script>
  import { onMount, onDestroy } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtTime, fmtRelative, statusBadge, matchesFilter } from '../lib/utils.js';

  let jobs = [], loading = true, error = '';
  let filterQuery = '';
  // jobName → Run[] (アクティブRunのみ)
  let activeRunsByJob = {};
  // 展開中のジョブ名
  let expandedJob = null;
  // 展開中ジョブの直近runs（最大5件）
  let expandedRuns = [];
  let expandedLoading = false;

  $: filteredJobs = jobs.filter((j) => matchesFilter(j.name, filterQuery));

  async function load() {
    loading = true; error = '';
    try { jobs = await apiFetch('/api/v1/jobs'); }
    catch (e) { error = e.message; }
    finally { loading = false; }
  }

  async function refreshActive() {
    try {
      const runs = await apiFetch('/api/v1/runs/active');
      const byJob = {};
      for (const r of runs) {
        if (!byJob[r.jobName]) byJob[r.jobName] = [];
        byJob[r.jobName].push(r);
      }
      activeRunsByJob = byJob;
    } catch (_) { /* ポーリング失敗は無視 */ }
  }

  async function toggleExpand(jobName) {
    if (expandedJob === jobName) {
      expandedJob = null;
      expandedRuns = [];
      return;
    }
    expandedJob = jobName;
    expandedLoading = true;
    expandedRuns = [];
    try {
      const runs = await apiFetch('/api/v1/runs?jobName=' + encodeURIComponent(jobName));
      expandedRuns = runs.slice(0, 5);
    } catch (_) {
      expandedRuns = [];
    } finally {
      expandedLoading = false;
    }
  }

  let pollInterval;
  onMount(async () => {
    await load();
    await refreshActive();
    pollInterval = setInterval(refreshActive, 5000);
  });
  onDestroy(() => clearInterval(pollInterval));
</script>

<div class="container">
  <AuthSetup />
  <h1>Jobs</h1>
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !jobs.length}<div class="empty">No jobs yet. Apply a job YAML to get started.</div>
  {:else}
  <input
    type="text"
    class="filter-input"
    placeholder="Filter jobs..."
    bind:value={filterQuery}
  />
  {#if !filteredJobs.length}
    <div class="empty">No jobs match "{filterQuery}".</div>
  {:else}
  <table>
    <thead><tr><th>Name</th><th>Updated</th><th></th></tr></thead>
    <tbody>
      {#each filteredJobs as j (j.name)}
        <tr
          style="cursor:pointer"
          on:click={() => toggleExpand(j.name)}
        >
          <td>
            <a href="#/jobs/{j.name}" on:click|stopPropagation>{j.name}</a>
            {#if activeRunsByJob[j.name]?.length}
              <span class="badge badge-running" style="margin-left:0.5rem">
                ● 実行中 {activeRunsByJob[j.name].length > 1 ? `(${activeRunsByJob[j.name].length})` : ''}
              </span>
            {/if}
          </td>
          <td class="meta">{fmtTime(j.updatedAt)}</td>
          <td><a href="#/jobs/{j.name}" class="btn" on:click|stopPropagation>Runs →</a></td>
        </tr>
        {#if expandedJob === j.name}
          <tr>
            <td colspan="3" style="padding:0">
              <div style="background:var(--bg-elev);border-top:1px solid var(--border);padding:0.5rem 1rem">
                {#if expandedLoading}
                  <div class="meta" style="padding:0.25rem 0">Loading...</div>
                {:else if !expandedRuns.length}
                  <div class="meta" style="padding:0.25rem 0">Runはありません。</div>
                {:else}
                  {#each expandedRuns as r (r.id)}
                    <div
                      style="display:flex;align-items:center;gap:0.75rem;padding:0.3rem 0;cursor:pointer"
                      on:click|stopPropagation={() => { window.location.hash = '/runs/' + r.id; }}
                    >
                      <span class={statusBadge(r.status)}>{r.status}</span>
                      <span class="meta" style="font-family:monospace;font-size:0.8rem">{r.id.slice(0,8)}</span>
                      <span class="meta">{fmtRelative(r.createdAt)}</span>
                    </div>
                  {/each}
                  <div style="margin-top:0.25rem">
                    <a href="#/jobs/{j.name}" class="meta" style="font-size:0.8rem" on:click|stopPropagation>すべて見る →</a>
                  </div>
                {/if}
              </div>
            </td>
          </tr>
        {/if}
      {/each}
    </tbody>
  </table>
  {/if}
  {/if}
</div>
