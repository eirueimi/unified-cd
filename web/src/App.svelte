<script>
  import Router, { router } from "svelte-spa-router";
  import { onMount } from "svelte";
  import { initAuth, serverURL, browserSSOEnabled, currentUser, token, saveAuth, authReady, savePostLoginHash, restorePostLoginHash } from "./lib/api.js";
  import { themePref, toggleTheme, initTheme } from "./lib/theme.js";
  import JobList from "./routes/JobList.svelte";
  import JobDetail from "./routes/JobDetail.svelte";
  import JobRun from "./routes/JobRun.svelte";
  import JobYaml from "./routes/JobYaml.svelte";
  import RunDetail from "./routes/RunDetail.svelte";
  import RunYaml from "./routes/RunYaml.svelte";
  import AgentMonitor from "./routes/AgentMonitor.svelte";
  import AgentDetail from "./routes/AgentDetail.svelte";
  import TokenList from "./routes/TokenList.svelte";
  import ScheduleList from "./routes/ScheduleList.svelte";
  import WebhookList from "./routes/WebhookList.svelte";
  import GitCredentialList from "./routes/GitCredentialList.svelte";
  import AppSourceList from "./routes/AppSourceList.svelte";
  import SecretList from "./routes/SecretList.svelte";

  const routes = {
    "/": JobList,
    "/jobs/:name": JobDetail,
    "/jobs/:name/run": JobRun,
    "/jobs/:name/yaml": JobYaml,
    "/runs/:id": RunDetail,
    "/runs/:id/yaml": RunYaml,
    "/agents": AgentMonitor,
    "/agents/:id": AgentDetail,
    "/tokens": TokenList,
    "/resources/schedules": ScheduleList,
    "/resources/webhooks": WebhookList,
    "/resources/gitcredentials": GitCredentialList,
    "/resources/appsources": AppSourceList,
    "/resources/secrets": SecretList,
  };

  onMount(() => {
    initAuth().then(() => {
      // Restore a hash route saved before an SSO redirect (deep-link across
      // the full-page navigation to the IdP and back). Only ever restores an
      // in-app "#/..." route — see restorePostLoginHash.
      restorePostLoginHash();
    });
    initTheme();
  });

  function ssoLogin() {
    savePostLoginHash();
    window.location.href = $serverURL + '/api/v1/auth/oidc-login';
  }

  async function logout() {
    await fetch($serverURL + '/api/v1/auth/logout', { method: 'POST', credentials: 'include' });
    currentUser.set(null);
    token.set('');
    saveAuth('', $serverURL);
  }
</script>

<nav>
  <span class="brand">⚡ unified-cd</span>
  <a href="#/">Jobs</a>
  <a href="#/agents">Agents</a>
  <div class="dropdown">
    <span class="dropdown-trigger" class:nav-active={router.location.startsWith('/resources/')} tabindex="0" role="button" aria-haspopup="true">Resources ▾</span>
    <div class="dropdown-menu">
      <a href="#/resources/schedules">Schedules</a>
      <a href="#/resources/webhooks">Webhooks</a>
      <a href="#/resources/gitcredentials">GitCredentials</a>
      <a href="#/resources/appsources">AppSources</a>
      <a href="#/resources/secrets">Secrets</a>
    </div>
  </div>
  <a href="#/tokens">Tokens</a>
  <span class="spacer"></span>
  <button
    class="btn theme-toggle"
    on:click={toggleTheme}
    title={$themePref === 'light' ? 'Theme: Light (click for Dark)' : 'Theme: Dark (click for Light)'}
    aria-label="Toggle theme"
  >{$themePref === 'light' ? '☀️' : '🌙'}</button>
  {#if $currentUser}
    <span class="meta" style="font-size:0.75rem">{$currentUser.email}</span>
    <button class="btn btn-danger" on:click={logout} style="font-size:0.75rem;padding:0.2rem 0.6rem">Log out</button>
  {:else if $browserSSOEnabled}
    <button class="btn" on:click={ssoLogin} style="font-size:0.75rem;padding:0.2rem 0.6rem;background:var(--success-soft-bg);border-color:var(--success);color:var(--success)">🔒 Log in with SSO</button>
  {/if}
  <span class="meta" style="font-size:0.75rem">{$serverURL}</span>
</nav>
{#if $authReady}
  <Router {routes} />
{:else}
  <div class="container"><div class="loading">Loading...</div></div>
{/if}
