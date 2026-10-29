<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch, grants, currentUser } from '../lib/api.js';
  import { allows, requiresText } from '../lib/authz.js';
  import { fmtTime } from '../lib/utils.js';

  let tokens = [], loading = true, error = '', createError = '';
  let newName = '', newExpiry = '', newRole = '';
  let createdToken = null;
  let chosenPerms = [];

  // The server enforces the ceiling (a role above the caller's own, or a
  // permission the caller does not hold, is a 400); the form only offers what
  // the caller can legitimately pick.
  const ROLES = ['viewer', 'developer', 'admin'];
  $: myRank = ROLES.indexOf($currentUser?.role) + 1;
  $: roleOptions = $currentUser ? ROLES.filter((_, i) => i + 1 <= myRank) : ROLES;
  $: heldPerms = $grants
    ? [...new Set([...($grants.global || []), ...Object.values($grants.folders || {}).flat()])].sort()
    : [];
  $: canCreate = allows($grants, 'token.create');
  $: canManage = $grants != null && allows($grants, 'token.manage');

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
      if (chosenPerms.length) body.permissions = chosenPerms;
      else if (newRole) body.role = newRole;
      const resp = await apiFetch('/api/v1/tokens', {
        method: 'POST',
        body: JSON.stringify(body),
      });
      createdToken = resp.token;
      newName = ''; newExpiry = ''; newRole = ''; chosenPerms = [];
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
      <select class="token-input" bind:value={newRole} style="flex:1;min-width:120px" disabled={chosenPerms.length > 0} title="Cannot exceed your own role; choose permissions instead for a narrower token">
        <option value="">Role (default: yourself)</option>
        {#each roleOptions as r}<option value={r}>{r}</option>{/each}
      </select>
      <button class="btn" disabled={!canCreate} title={canCreate ? '' : requiresText('token.create')} on:click={create}>Issue</button>
    </div>
    {#if heldPerms.length}
    <fieldset style="margin-top:0.75rem;border:1px solid var(--border);border-radius:4px;padding:0.5rem">
      <legend class="meta" style="font-size:0.8rem">Or limit to specific permissions (replaces the role)</legend>
      <div style="display:flex;flex-wrap:wrap;gap:0.25rem 1rem">
        {#each heldPerms as p (p)}
          <label style="font-size:0.8rem;cursor:pointer"><input type="checkbox" value={p} bind:group={chosenPerms} /> {p}</label>
        {/each}
      </div>
    </fieldset>
    {/if}
    {#if createError}<div class="error" style="margin-top:0.5rem">{createError}</div>{/if}
  </div>

  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !tokens.length}<div class="empty">No tokens.</div>
  {:else}
  <h2 style="margin-bottom:0.5rem">{canManage ? 'All tokens' : 'Your tokens'}</h2>
  <table>
    <thead>
      <tr>
        <th>Name</th>
        {#if canManage}<th>Owner</th>{/if}
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
          {#if canManage}<td class="meta">{t.owner || '—'}</td>{/if}
          <td class="meta">{t.role || (t.permissions ? t.permissions.join(', ') : '')}</td>
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
