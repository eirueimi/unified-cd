<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtTime } from '../lib/utils.js';

  let webhooks = [], loading = true, error = '';

  async function load() {
    loading = true; error = '';
    try { webhooks = await apiFetch('/api/v1/webhooks'); }
    catch (e) { error = e.message; }
    finally { loading = false; }
  }

  function copyEndpoint(name) {
    navigator.clipboard.writeText('/webhook/' + name).catch(() => {});
  }

  onMount(load);
</script>

<div class="container">
  <AuthSetup />
  <h1>Webhooks</h1>
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !webhooks.length}<div class="empty">No webhooks yet.</div>
  {:else}
  <table>
    <thead>
      <tr><th>Name</th><th>Trigger job</th><th>Auth</th><th>Endpoint</th><th>Updated</th></tr>
    </thead>
    <tbody>
      {#each webhooks as w (w.name)}
        <tr>
          <td>{w.name}</td>
          <td><a href="#/jobs/{w.jobName}">{w.jobName}</a></td>
          <td><span class="badge badge-running">{w.authType || 'none'}</span></td>
          <td>
            <code class="meta" style="font-size:0.8rem">/webhook/{w.name}</code>
            <button class="btn" style="padding:0.15rem 0.4rem;font-size:0.75rem;margin-left:0.25rem" on:click={() => copyEndpoint(w.name)}>copy</button>
          </td>
          <td class="meta">{fmtTime(w.updatedAt)}</td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
