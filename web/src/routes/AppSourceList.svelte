<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtRelative } from '../lib/utils.js';

  let sources = [], loading = true, error = '';
  let syncing = {};

  async function load() {
    loading = true; error = '';
    try { sources = await apiFetch('/api/v1/appsources'); }
    catch (e) { error = e.message; }
    finally { loading = false; }
  }

  async function sync(name) {
    syncing = { ...syncing, [name]: true };
    try {
      await apiFetch(`/api/v1/appsources/${name}/sync`, { method: 'POST' });
      await load();
    } catch (e) { error = e.message; }
    finally { syncing = { ...syncing, [name]: false }; }
  }

  onMount(load);
</script>

<div class="container">
  <AuthSetup />
  <h1>AppSources</h1>
  {#if error}<div class="error" style="margin-bottom:0.75rem">{error}</div>{/if}
  {#if loading}<div class="loading">Loading...</div>
  {:else if !sources.length}<div class="empty">No app sources yet.</div>
  {:else}
  <table>
    <thead>
      <tr><th>Name</th><th>Repo</th><th>Ref</th><th>Path</th><th>Last synced</th><th>Commit</th><th></th></tr>
    </thead>
    <tbody>
      {#each sources as s (s.name)}
        <tr>
          <td>{s.name}</td>
          <td><a href={s.repoURL} target="_blank" rel="noreferrer">{s.repoURL.replace(/^https?:\/\//, '')}</a></td>
          <td><code>{s.targetRevision}</code></td>
          <td class="meta">{s.path || '/'}</td>
          <td class="meta">{s.lastSyncedAt ? fmtRelative(s.lastSyncedAt) : '—'}</td>
          <td><code class="meta">{s.lastCommit ? s.lastCommit.slice(0, 7) : '—'}</code></td>
          <td>
            <button class="btn" disabled={syncing[s.name]} on:click={() => sync(s.name)}>
              {syncing[s.name] ? '...' : '↺ Sync'}
            </button>
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
