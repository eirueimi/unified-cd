<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtTime } from '../lib/utils.js';

  let secrets = [], loading = true, error = '';

  const scopeBadge = {
    global: 'badge badge-queued',
    job:    'badge badge-running',
    agent:  'badge badge-success',
  };

  async function load() {
    loading = true; error = '';
    try { secrets = await apiFetch('/api/v1/secrets'); }
    catch (e) { error = e.message; }
    finally { loading = false; }
  }
  onMount(load);
</script>

<div class="container">
  <AuthSetup />
  <h1>Secrets</h1>
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !secrets.length}<div class="empty">No secrets yet.</div>
  {:else}
  <table>
    <thead>
      <tr><th>Name</th><th>Scope</th><th>Scope ref</th><th>Created</th></tr>
    </thead>
    <tbody>
      {#each secrets as s (s.id)}
        <tr>
          <td>{s.name}</td>
          <td><span class={scopeBadge[s.scope] || 'badge badge-pending'}>{s.scope}</span></td>
          <td class="meta">{s.scopeRef || '—'}</td>
          <td class="meta">{fmtTime(s.createdAt)}</td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
