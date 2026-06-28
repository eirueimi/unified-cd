<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtTime } from '../lib/utils.js';

  let tokens = [], loading = true, error = '', createError = '';
  let newName = '', newExpiry = '';
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
    if (!name) { createError = '名前を入力してください'; return; }
    try {
      const body = { name };
      if (newExpiry.trim()) body.expiresIn = newExpiry.trim();
      const resp = await apiFetch('/api/v1/tokens', {
        method: 'POST',
        body: JSON.stringify(body),
      });
      createdToken = resp.token;
      newName = ''; newExpiry = '';
      await load();
    } catch (e) { createError = e.message; }
  }

  async function remove(id) {
    if (!confirm('このトークンを削除しますか？')) return;
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
    <h2 style="color:var(--success)">トークンが発行されました</h2>
    <p style="font-size:0.8rem;color:var(--text-muted);margin-bottom:0.5rem">
      このトークンは一度しか表示されません。今すぐコピーしてください。
    </p>
    <div style="display:flex;gap:0.5rem;align-items:center">
      <code style="flex:1;background:var(--bg-inset);padding:0.5rem;border-radius:4px;word-break:break-all;font-size:0.85rem">{createdToken}</code>
      <button class="btn" on:click={copyToken}>コピー</button>
      <button class="btn btn-danger" on:click={() => createdToken = null}>閉じる</button>
    </div>
  </div>
  {/if}

  <div class="card" style="margin-bottom:1rem">
    <h2>新規トークン発行</h2>
    <div style="display:flex;gap:0.5rem;margin-top:0.75rem;flex-wrap:wrap">
      <input class="token-input" bind:value={newName} placeholder="名前（例: ci-deploy）" style="flex:2;min-width:150px"/>
      <input class="token-input" bind:value={newExpiry} placeholder="有効期限（例: 24h, 720h）省略可" style="flex:1;min-width:120px"/>
      <button class="btn" on:click={create}>発行</button>
    </div>
    {#if createError}<div class="error" style="margin-top:0.5rem">{createError}</div>{/if}
  </div>

  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !tokens.length}<div class="empty">トークンがありません。</div>
  {:else}
  <table>
    <thead>
      <tr>
        <th>名前</th>
        <th>作成日時</th>
        <th>有効期限</th>
        <th></th>
      </tr>
    </thead>
    <tbody>
      {#each tokens as t (t.id)}
        <tr>
          <td>{t.name}</td>
          <td class="meta">{fmtTime(t.createdAt)}</td>
          <td class="meta">{t.expiresAt ? fmtTime(t.expiresAt) : '無期限'}</td>
          <td>
            <button class="btn btn-danger" on:click={() => remove(t.id)}>削除</button>
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
