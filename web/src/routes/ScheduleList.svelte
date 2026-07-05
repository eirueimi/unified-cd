<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch } from '../lib/api.js';
  import { fmtRelative, fmtTime } from '../lib/utils.js';

  let schedules = [], loading = true, error = '';

  async function load() {
    loading = true; error = '';
    try { schedules = await apiFetch('/api/v1/schedules'); }
    catch (e) { error = e.message; }
    finally { loading = false; }
  }
  onMount(load);
</script>

<div class="container">
  <AuthSetup />
  <h1>Schedules</h1>
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if !schedules.length}<div class="empty">No schedules yet.</div>
  {:else}
  <table>
    <thead>
      <tr><th>Name</th><th>Cron</th><th>Job</th><th>Last fired</th><th>Updated</th></tr>
    </thead>
    <tbody>
      {#each schedules as s (s.name)}
        <tr>
          <td>{s.name}</td>
          <td><code>{s.cron}</code></td>
          <td><a href="#/jobs/{encodeURIComponent(s.jobName)}">{s.jobName}</a></td>
          <td class="meta">{s.lastFiredAt ? fmtRelative(s.lastFiredAt) : '—'}</td>
          <td class="meta">{fmtTime(s.updatedAt)}</td>
        </tr>
      {/each}
    </tbody>
  </table>
  {/if}
</div>
