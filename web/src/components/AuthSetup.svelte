<script>
  import { token, serverURL, browserSSOEnabled, currentUser, saveAuth } from '../lib/api.js';

  let localToken = $token;
  let localServer = $serverURL;
  let saveError = '';
  let saving = false;

  async function save() {
    saveError = '';
    saving = true;
    token.set(localToken);
    serverURL.set(localServer);
    saveAuth(localToken, localServer);
    try {
      const headers = localToken ? { Authorization: 'Bearer ' + localToken } : {};
      const resp = await fetch(localServer + '/api/v1/auth/me', { headers, credentials: 'include' });
      if (resp.ok) {
        window.location.reload();
        return;
      }
      saveError = 'Invalid token';
    } catch {
      saveError = 'Could not reach server';
    } finally {
      saving = false;
    }
  }
  function ssoLogin() {
    window.location.href = $serverURL + '/api/v1/auth/oidc-login';
  }
</script>

{#if !$currentUser}
<div class="card" style="margin-bottom:1rem">
  <h2>Connection</h2>
  {#if $browserSSOEnabled}
    <div style="margin-top:0.5rem;margin-bottom:0.75rem">
      <button class="btn" on:click={ssoLogin} style="background:var(--success-soft-bg);border-color:var(--success);color:var(--success)">
        SSO Login
      </button>
    </div>
    <div style="font-size:0.75rem;color:var(--text-muted);margin-bottom:0.5rem">── or enter token manually ──</div>
  {/if}
  <div class="token-form">
    <input class="token-input" bind:value={localServer} placeholder="Server URL" style="flex:2"/>
    <input class="token-input" bind:value={localToken} type="password" placeholder="Bearer token or PAT"/>
    <button class="btn" on:click={save} disabled={saving}>{saving ? 'Checking…' : 'Save'}</button>
  </div>
  {#if saveError}
    <div class="error" style="margin-top:0.5rem;font-size:0.85rem">{saveError}</div>
  {/if}
</div>
{/if}
