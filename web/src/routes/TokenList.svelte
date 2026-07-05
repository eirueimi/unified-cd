<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtTime } from '../lib/utils.js';

  let tokens = [], loading = true, error = '', createError = '';
  let newName = '', newExpiry = '', newRole = '';
  let createdToken = null;

  async function load() {
    loading = true; error = '';
    try { tokens = await apiFetch('/api/v1/tokens'); }
    catch (e) { error = e.message; }
    finally { loading = false; }
  }

  async function create() {
    createError = ''; createdToken = null;
    const name = newName.trim();
    if (!name) { createError = 'Please enter a name'; return; }
    try {
      const body = { name };
      if (newExpiry.trim()) body.expiresIn = newExpiry.trim();
      if (newRole) body.role = newRole;
      const resp = await apiFetch('/api/v1/tokens', {
        method: 'POST',
        body: JSON.stringify(body),
      });
      createdToken = resp.token;
      newName = ''; newExpiry = ''; newRole = '';
      await load();
    } catch (e) { createError = e.message; }
  }

  async function remove(id) {
    if (!confirm('Delete this token?')) return;
    try {
      await apiFetch(`/api/v1/tokens/${id}`, { method: 'DELETE' });
      await load();
    } catch (e) { error = e.message; }
  }

  function copyToken() {
    navigator.clipboard.writeText(createdToken).catch(() => {});
  }

  onMount(load);
</script>

<div class="container">
  <AuthSetup />
  <h1>Personal Access Tokens</h1>

  {#if createdToken}
  <div class="card" style="margin-bottom:1rem;border-color:var(--success)">
    <h2 style="color:var(--success)">Token issued</h2>
    <p style="font-size:0.8rem;color:var(--text-muted);margin-bottom:0.5rem">
      This token will only be shown once. Copy it now.
    </p>
    <div style="display:flex;gap:0.5rem;align-items:center">
      <code style="flex:1;background:var(--bg-inset);padding:0.5rem;border-radius:4px;word-break:break-all;font-size:0.85rem">{createdToken}</code>
      <button class="btn" on:click={copyToken}>Copy</button>
      <button class="btn btn-danger" on:click={() => createdToken = null}>Close</button>
    </div>
  </div>
  {/if}

  <div class="card" style="margin-bottom:1rem">
    <h2>Issue New Token</h2>
    <div style="display:flex;gap:0.5rem;margin-top:0.75rem;flex-wrap:wrap">
      <input class="token-input" bind:value={newName} placeholder="Name (e.g. ci-deploy)" style="flex:2;min-width:150px"/>
      <input class="token-input" bind:value={newExpiry} placeholder="Expiry (e.g. 24h, 720h), optional" style="flex:1;min-width:120px"/>
      <select class="token-input" bind:value={newRole} style="flex:1;min-width:120px" title="Will be capped at the issuer's role">
        <option value="">Role (default: yourself)</option>
        <option value="viewer">viewer</option>
        <option value="developer">developer</option>
        <option value="admin">admin</option>
      </select>
      <button class="btn" on:click={create}>Issue</button>
    </div>
    {#if createError}<div class="error" style="margin-top:0.5rem">{createError}</div>{/if}
  </div>

  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !tokens.length}<div class="empty">No tokens.</div>
  {:else}
  <table>
    <thead>
      <tr>
        <th>Name</th>
        <th>Role</th>
        <th>Created</th>
        <th>Expires</th>
        <th></th>
      </tr>
    </thead>
    <tbody>
      {#each tokens as t (t.id)}
        <tr>
          <td>{t.name}</td>
          <td class="meta">{t.role}</td>
          <td class="meta">{fmtTime(t.createdAt)}</td>
          <td class="meta">{t.expiresAt ? fmtTime(t.expiresAt) : 'No expiry'}</td>
          <td>
            <button class="btn btn-danger" on:click={() => remove(t.id)}>Delete</button>
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
