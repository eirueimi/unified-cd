<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtTime } from '../lib/utils.js';

  let creds = [], loading = true, error = '';

  async function load() {
    loading = true; error = '';
    try { creds = await apiFetch('/api/v1/gitcredentials'); }
    catch (e) { error = e.message; }
    finally { loading = false; }
  }
  onMount(load);
</script>

<div class="container">
  <AuthSetup />
  <h1>GitCredentials</h1>
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !creds.length}<div class="empty">No git credentials yet.</div>
  {:else}
  <table>
    <thead>
      <tr><th>Name</th><th>Host</th><th>Type</th><th>Secret ref</th><th>Created</th></tr>
    </thead>
    <tbody>
      {#each creds as c (c.id)}
        <tr>
          <td>{c.name}</td>
          <td>{c.host}</td>
          <td><span class="badge badge-success">{c.credType}</span></td>
          <td class="meta">{c.secretRef}</td>
          <td class="meta">{fmtTime(c.createdAt)}</td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
