<script>
  import { onMount, onDestroy } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtRelative } from '../lib/utils.js';

  let agents = [], loading = true, error = '', timer;

  function isOnline(ts) { return Date.now() - new Date(ts).getTime() < 90000; }

  async function load() {
    try { agents = await apiFetch('/api/v1/agents'); }
    catch (e) { if (!agents.length) error = e.message; }
    finally { loading = false; }
  }

  onMount(() => { load(); timer = setInterval(load, 10000); });
  onDestroy(() => clearInterval(timer));
</script>

<div class="container">
  <AuthSetup />
  <h1>Agent Monitor <span class="meta" style="font-size:0.85rem">(refreshes every 10s)</span></h1>
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !agents.length}<div class="empty">No agents registered yet.</div>
  {:else}
  <table>
    <thead><tr><th>ID</th><th>Hostname</th><th>OS</th><th>Labels</th><th>Status</th><th>Last seen</th></tr></thead>
    <tbody>
      {#each agents as a (a.id)}
        <tr style="cursor:pointer" on:click={() => window.location.hash = '/agents/' + encodeURIComponent(a.id)}>
          <td style="font-family:monospace;font-size:0.8rem">{a.id}</td>
          <td>{a.hostname}</td><td>{a.os}</td>
          <td class="meta">
            {#if (a.labels || []).length === 0}
              —
            {:else if (a.labels || []).length === 1}
              {a.labels[0]}
            {:else}
              {a.labels[0]} <span class="meta">+{a.labels.length - 1}</span>
            {/if}
          </td>
          <td><span class={isOnline(a.lastSeenAt) ? 'badge badge-success' : 'badge badge-failed'}>{isOnline(a.lastSeenAt) ? 'Online' : 'Offline'}</span></td>
          <td class="meta">{fmtRelative(a.lastSeenAt)}</td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
