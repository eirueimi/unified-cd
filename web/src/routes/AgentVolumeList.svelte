<script>
  import { onMount } from 'svelte';
  import AuthSetup from '../components/AuthSetup.svelte';
  import { apiFetch, grants } from '../lib/api.js';
  import { allows } from '../lib/authz.js';
  import { fmtRelative } from '../lib/utils.js';
  import { fmtBytes, aggregateVolume } from '../lib/volumes.js';

  let decls = [], agents = [], loading = true, error = '';
  let expanded = {};

  // Per-agent columns need agent.read; without it the page is declarations only.
  $: canAgents = allows($grants, 'agent.read');
  $: summaries = Object.fromEntries(decls.map((d) => [d.name, aggregateVolume(d.name, agents)]));

  async function load() {
    loading = true; error = '';
    try {
      decls = (await apiFetch('/api/v1/agent-volumes')) || [];
      agents = canAgents ? (await apiFetch('/api/v1/agents')) || [] : [];
    } catch (e) { error = e.message; }
    finally { loading = false; }
  }

  function toggle(name) {
    if (!canAgents) return;
    expanded = { ...expanded, [name]: !expanded[name] };
  }

  onMount(load);
</script>

<div class="container">
  <AuthSetup />
  <h1>Volumes</h1>
  {#if error}<div class="error" style="margin-bottom:0.75rem">{error}</div>{/if}
  {#if loading}<div class="loading">Loading...</div>
  {:else if !decls.length}<div class="empty">No volumes declared yet.</div>
  {:else}
  <table class="cards">
    <thead>
      <tr>
        <th>Volume</th><th>Mode</th><th>Size</th><th>Selector</th><th>Reclaim</th>
        {#if canAgents}<th>Agents</th><th>Total used</th><th>Largest</th>{/if}
      </tr>
    </thead>
    <tbody>
      {#each decls as d (d.name)}
        {@const s = summaries[d.name]}
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
        <tr class:clickable={canAgents} on:click={() => toggle(d.name)}>
          <td data-label="Volume" style="font-family:monospace;font-size:0.85rem">
            <span>{#if canAgents}<button type="button" class="vol-toggle" aria-expanded={!!expanded[d.name]} aria-label={`Toggle agents for ${d.name}`}>{expanded[d.name] ? '▾' : '▸'}</button> {/if}<span>{d.name}</span></span>
          </td>
          <td data-label="Mode"><span>{d.spec?.mode ?? '—'}</span></td>
          <td data-label="Size"><span>{d.spec?.size ?? '—'}</span></td>
          <td data-label="Selector"><span>{(d.spec?.agentSelector || []).join(', ') || '—'}</span></td>
          <td data-label="Reclaim"><span>{d.spec?.reclaimPolicy || 'Retain'}</span></td>
          {#if canAgents}
            <td data-label="Agents"><span>{s.ready} / {s.asked}</span></td>
            <td data-label="Total used">
              <span>{fmtBytes(s.totalUsed)}{#if s.totalUsed != null && s.measured < s.asked} <span class="meta">({s.measured} of {s.asked} measured)</span>{/if}</span>
            </td>
            <td data-label="Largest">
              <span>
                {#if s.largest}
                  <a href={`#/agents/${s.largest.agentId}`} on:click|stopPropagation>{s.largest.agentId}</a> {fmtBytes(s.largest.usedBytes)}
                {:else}—{/if}
              </span>
            </td>
          {/if}
        </tr>
        {#if canAgents && expanded[d.name]}
          <tr class="vol-detail">
            <td data-label="Agents" colspan="8">
              <div style="width:100%">
                {#if !s.perAgent.length}
                  <div class="empty">No agent has been asked to provision this volume.</div>
                {:else}
                  <table class="cards">
                    <thead><tr><th>Agent</th><th>State</th><th>Used</th><th>Disk free</th><th>Measured</th></tr></thead>
                    <tbody>
                      {#each s.perAgent as a (a.agentId)}
                        <tr class:vol-offline={!a.online}>
                          <td data-label="Agent" style="font-family:monospace;font-size:0.85rem">
                            <span><a href={`#/agents/${a.agentId}`}>{a.agentId}</a>{#if !a.online} <span class="badge badge-failed">Offline</span>{/if}</span>
                          </td>
                          <td data-label="State">
                            <span>
                              {#if a.ready}<span class="badge badge-success">Ready</span>
                              {:else}<span class="badge badge-failed">Not ready</span>{#if a.reason} <span class="meta">{a.reason}</span>{/if}{/if}
                            </span>
                          </td>
                          <td data-label="Used"><span>{fmtBytes(a.usedBytes)}</span></td>
                          <td data-label="Disk free" class:vol-blocked={a.claimBlocked}><span>{a.freeBytes != null && a.totalBytes != null ? fmtBytes(a.freeBytes) + ' / ' + fmtBytes(a.totalBytes) : '—'}</span></td>
                          <td data-label="Measured" class="meta"><span>{a.measuredAt ? fmtRelative(a.measuredAt) : '—'}</span></td>
                        </tr>
                      {/each}
                    </tbody>
                  </table>
                {/if}
              </div>
            </td>
          </tr>
        {/if}
      {/each}
    </tbody>
  </table>
  {/if}
</div>

<style>
  tr.clickable { cursor: pointer; }
  tr.vol-offline { opacity: 0.55; }
  .vol-toggle { background: none; border: none; padding: 0; color: var(--text-muted); cursor: pointer; font: inherit; }
</style>
