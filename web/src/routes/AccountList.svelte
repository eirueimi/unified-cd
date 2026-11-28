<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtTime } from '../lib/utils.js';

  let accounts = [], loading = true, error = '', actionError = '';

  async function load() {
    loading = true; error = '';
    try { accounts = (await apiFetch('/api/v1/accounts')) || []; }
    catch (e) { error = e.message; }
    finally { loading = false; }
  }

  // Service accounts are enabled and disabled in the controller config, so only
  // human accounts get the buttons.
  async function disable(a) {
    actionError = '';
    try {
      await apiFetch(`/api/v1/accounts/${encodeURIComponent(a.id)}/disable`, { method: 'POST' });
      await load();
    } catch (e) { actionError = e.message; }
  }

  async function enable(a) {
    actionError = '';
    try {
      await apiFetch(`/api/v1/accounts/${encodeURIComponent(a.id)}/enable`, { method: 'POST' });
      await load();
    } catch (e) { actionError = e.message; }
  }

  onMount(load);
</script>

<div class="container">
  <AuthSetup />
  <h1>Accounts</h1>
  {#if actionError}<div class="error" style="margin-bottom:0.5rem">{actionError}</div>{/if}
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !accounts.length}<div class="empty">No accounts.</div>
  {:else}
  <table>
    <thead>
      <tr>
        <th>Name</th>
        <th>Kind</th>
        <th>Status</th>
        <th>Tokens</th>
        <th>Last seen</th>
        <th></th>
      </tr>
    </thead>
    <tbody>
      {#each accounts as a (a.id)}
        <tr>
          <td>{a.name}</td>
          <td class="meta">{a.kind}</td>
          <td class="meta">{a.disabled ? 'Disabled' : 'Active'}</td>
          <td class="meta">{a.patCount ?? 0}</td>
          <td class="meta">{a.lastSeenAt ? fmtTime(a.lastSeenAt) : '—'}</td>
          <td>
            {#if a.kind === 'human'}
              {#if a.disabled}
                <button class="btn" on:click={() => enable(a)}>Enable</button>
              {:else}
                <button class="btn btn-danger" on:click={() => disable(a)}>Disable</button>
              {/if}
            {/if}
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
