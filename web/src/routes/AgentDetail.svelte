<script>
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { statusBadge, fmtRelative } from '../lib/utils.js';

  export let params;
  $: agentId = params.id;

  let agent = null, runs = [], loading = true, error = '';

  function isOnline(ts) { return Date.now() - new Date(ts).getTime() < 90000; }

  async function load() {
    loading = true; error = '';
    try {
      agent = await apiFetch('/api/v1/agents/' + encodeURIComponent(agentId));
    } catch (e) {
      if (e.message === 'agent not found') {
        agent = null;
      } else {
        error = e.message;
        loading = false;
        return;
      }
    }
    if (agent !== null) {
      try {
        runs = await apiFetch('/api/v1/agents/' + encodeURIComponent(agentId) + '/runs') || [];
      } catch (e) {
        runs = [];
      }
    }
    loading = false;
  }

  $: agentId, load();
</script>

<div class="container">
  <AuthSetup />
  <div style="display:flex;align-items:center;gap:1rem;margin-bottom:1rem">
    <a href="#/agents">← Agents</a>
    <h1>{agentId}</h1>
    {#if agent}
      <span class={isOnline(agent.lastSeenAt) ? 'badge badge-success' : 'badge badge-failed'}>
        {isOnline(agent.lastSeenAt) ? 'Online' : 'Offline'}
      </span>
    {/if}
  </div>

  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !agent}<div class="empty">このエージェントは削除されています。</div>
  {:else}

  <div class="card grid-2" style="margin-bottom:1rem">
    <div><div class="meta">Hostname</div><div style="font-family:monospace;font-size:0.85rem">{agent.hostname}</div></div>
    <div><div class="meta">OS</div><div>{agent.os}</div></div>
    <div><div class="meta">Version</div><div style="font-family:monospace;font-size:0.85rem">{agent.version || '—'}</div></div>
    <div><div class="meta">Last seen</div><div>{fmtRelative(agent.lastSeenAt)}</div></div>
  </div>

  {#if (agent.labels || []).length > 0}
  <div class="card" style="margin-bottom:1rem">
    <div class="meta" style="margin-bottom:0.5rem">Labels</div>
    <div style="display:flex;flex-wrap:wrap;gap:0.4rem">
      {#each agent.labels as label}
        <span style="background:var(--bg-elev);border:1px solid var(--border);padding:0.15rem 0.6rem;border-radius:1rem;font-size:0.8rem;font-family:monospace">{label}</span>
      {/each}
    </div>
  </div>
  {/if}

  {#if agent.env && Object.keys(agent.env).length > 0}
  <div class="card" style="margin-bottom:1rem">
    <div class="meta" style="margin-bottom:0.5rem">Environment</div>
    <table>
      <thead><tr><th>Key</th><th>Value</th></tr></thead>
      <tbody>
        {#each Object.entries(agent.env).sort() as [k, v]}
          <tr>
            <td style="font-family:monospace;font-size:0.8rem;white-space:nowrap">{k}</td>
            <td style="font-family:monospace;font-size:0.8rem;word-break:break-all">{v}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  {/if}

  <div class="card">
    <div class="meta" style="margin-bottom:0.5rem">Recent Runs</div>
    {#if !runs.length}
      <div class="empty">No runs yet.</div>
    {:else}
      <table>
        <thead><tr><th>Run ID</th><th>Job</th><th>Status</th><th>Created</th></tr></thead>
        <tbody>
          {#each runs as r (r.id)}
            <tr style="cursor:pointer" on:click={() => window.location.hash = '/runs/' + r.id}>
              <td><a href="#/runs/{r.id}" style="font-family:monospace;font-size:0.8rem">{r.id.slice(0,8)}…</a></td>
              <td>{r.jobName}</td>
              <td><span class={statusBadge(r.status)}>{r.status}</span></td>
              <td class="meta">{fmtRelative(r.createdAt)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>

  {/if}
</div>
