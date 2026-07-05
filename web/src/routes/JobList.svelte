<script>
  import { onMount, onDestroy } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtTime, fmtRelative, statusBadge, buildJobTree, flattenJobTree } from '../lib/utils.js';

  let jobs = [], loading = true, error = '';
  let filterQuery = '';
  // jobName → Run[] (active Runs only)
  let activeRunsByJob = {};
  // Name of the currently expanded job
  let expandedJob = null;
  // Recent runs for the expanded job (up to 5)
  let expandedRuns = [];
  let expandedLoading = false;

  let collapsed = new Set();
  $: rows = flattenJobTree(buildJobTree(jobs), collapsed, filterQuery);

  function toggleFolder(path) {
    if (collapsed.has(path)) collapsed.delete(path); else collapsed.add(path);
    collapsed = collapsed;
  }

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
    } catch (_) { /* Ignore polling failures */ }
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
  {#if !rows.length}
    <div class="empty">No jobs match "{filterQuery}".</div>
  {:else}
  <table>
    <thead><tr><th>Name</th><th>Updated</th><th></th></tr></thead>
    <tbody>
      {#each rows as row (row.kind === 'folder' ? 'D:' + row.path : 'J:' + row.job.name)}
        {#if row.kind === 'folder'}
          <tr style="cursor:pointer" on:click={() => toggleFolder(row.path)}>
            <td colspan="3" style="padding-left:{0.75 + row.depth * 1.4}rem">
              <span class="meta">{collapsed.has(row.path) && !filterQuery ? '▸' : '▾'}</span>
              📁 {row.name}
            </td>
          </tr>
        {:else}
          <tr style="cursor:pointer" on:click={() => toggleExpand(row.job.name)}>
            <td style="padding-left:{0.75 + (row.depth + 1) * 1.4}rem">
              <a href="#/jobs/{encodeURIComponent(row.job.name)}" on:click|stopPropagation>{row.job.leaf}</a>
              {#if activeRunsByJob[row.job.name]?.length}
                <span class="badge badge-running" style="margin-left:0.5rem">
                  ● Running {activeRunsByJob[row.job.name].length > 1 ? `(${activeRunsByJob[row.job.name].length})` : ''}
                </span>
              {/if}
            </td>
            <td class="meta">{fmtTime(row.job.updatedAt)}</td>
            <td><a href="#/jobs/{encodeURIComponent(row.job.name)}" class="btn" on:click|stopPropagation>Runs →</a></td>
          </tr>
          {#if expandedJob === row.job.name}
            <tr>
              <td colspan="3" style="padding:0">
                <div style="background:var(--bg-elev);border-top:1px solid var(--border);padding:0.5rem 1rem">
                  {#if expandedLoading}
                    <div class="meta" style="padding:0.25rem 0">Loading...</div>
                  {:else if !expandedRuns.length}
                    <div class="meta" style="padding:0.25rem 0">No runs.</div>
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
                      <a href="#/jobs/{encodeURIComponent(row.job.name)}" class="meta" style="font-size:0.8rem" on:click|stopPropagation>View all →</a>
                    </div>
                  {/if}
                </div>
              </td>
            </tr>
          {/if}
        {/if}
      {/each}
    </tbody>
  </table>
  {/if}
  {/if}
</div>
