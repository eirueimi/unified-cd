<script>
  import { onMount, onDestroy, tick } from "svelte";
  import { push } from "svelte-spa-router";
  import { get } from "svelte/store";
  import AuthSetup from "../components/AuthSetup.svelte";
  import { apiFetch, token, serverURL } from "../lib/api.js";
  import { statusBadge, fmtTime } from "../lib/utils.js";

  export let params;
  $: runID = params.id;

  let run = null,
    logLines = [],
    steps = [],
    loading = true,
    error = "";
  let logBox,
    selectedStep = null,
    selectedParallelGroup = null,
    abortController = null,
    stepsTimer = null;

  $: groupedStages = (() => {
    const map = new Map();
    for (const s of steps) {
      if (!map.has(s.stageIndex)) map.set(s.stageIndex, []);
      map.get(s.stageIndex).push(s);
    }
    return [...map.entries()]
      .sort(([a], [b]) => a - b)
      .map(([stageIndex, stageSteps]) => ({ stageIndex, steps: stageSteps }));
  })();

  $: filteredLogs =
    selectedStep !== null
      ? logLines.filter((l) => l.stepIndex === selectedStep)
      : selectedParallelGroup !== null
      ? logLines.filter((l) => selectedParallelGroup.includes(l.stepIndex))
      : logLines;

  // Reactive so the log labels re-render when steps load after SSE starts.
  $: stepName = (idx) => {
    if (idx === -1) return "System";
    const s = steps.find((s) => s.index === idx);
    return s ? s.name : "step " + idx;
  };
  function stepDuration(s) {
    if (!s.startedAt) return "";
    const start = new Date(s.startedAt).getTime();
    const end = s.endedAt ? new Date(s.endedAt).getTime() : Date.now();
    const ms = end - start;
    if (ms < 1000) return ms + "ms";
    if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
    return (
      Math.floor(ms / 60000) + "m " + Math.floor((ms % 60000) / 1000) + "s"
    );
  }
  async function loadRun() {
    try {
      run = await apiFetch("/api/v1/runs/" + runID);
    } catch (e) {
      error = e.message;
    }
  }
  async function loadSteps() {
    try {
      steps = await apiFetch("/api/v1/runs/" + runID + "/steps");
    } catch {}
  }
  function stopStepPolling() {
    if (stepsTimer) {
      clearInterval(stepsTimer);
      stepsTimer = null;
    }
  }
  function startStepPolling() {
    stopStepPolling();
    stepsTimer = setInterval(async () => {
      await Promise.all([loadSteps(), loadRun()]);
      if (run && ["Succeeded", "Failed", "Cancelled"].includes(run.status))
        stopStepPolling();
    }, 3000);
  }
  async function startSSE() {
    if (abortController) {
      abortController.abort();
      abortController = null;
    }
    logLines = [];
    abortController = new AbortController();
    const headers = {};
    const t = get(token);
    if (t) headers["Authorization"] = "Bearer " + t;
    try {
      const resp = await fetch(
        get(serverURL) + "/api/v1/runs/" + runID + "/events",
        {
          credentials: "include",
          headers,
          signal: abortController.signal,
        },
      );
      if (!resp.ok || !resp.body) return;
      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buf = "";
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        const parts = buf.split("\n\n");
        buf = parts.pop();
        for (const part of parts) {
          const line = part.replace(/^data: /, "").trim();
          if (!line) continue;
          try {
            const data = JSON.parse(line);
            if (data.type === "log") {
              logLines = [...logLines, data];
              await tick();
              if (logBox && selectedStep === null)
                logBox.scrollTop = logBox.scrollHeight;
            } else if (data.type === "status") {
              if (run) run = { ...run, status: data.status };
              if (["Succeeded", "Failed", "Cancelled"].includes(data.status)) {
                await loadSteps();
                stopStepPolling();
                abortController.abort();
                return;
              }
            }
          } catch {}
        }
      }
    } catch (e) {
      if (e.name !== "AbortError") error = e.message;
    }
  }
  async function cancelRun() {
    try {
      await apiFetch("/api/v1/runs/" + runID + "/cancel", { method: "POST" });
      await loadRun();
    } catch (e) {
      error = e.message;
    }
  }
  async function rebuild() {
    try {
      const newRun = await apiFetch("/api/v1/runs", {
        method: "POST",
        body: JSON.stringify({ jobName: run.jobName, params: run.params }),
      });
      push("/runs/" + newRun.id);
    } catch (e) {
      error = e.message;
    }
  }
  function selectStep(idx) {
    selectedParallelGroup = null;
    selectedStep = selectedStep === idx ? null : idx;
  }

  async function init() {
    loading = true;
    await Promise.all([loadRun(), loadSteps()]);
    loading = false;
    if (run && !["Succeeded", "Failed", "Cancelled"].includes(run.status))
      startStepPolling();
    startSSE();
  }

  onMount(init);
  onDestroy(() => {
    if (abortController) abortController.abort();
    stopStepPolling();
  });
  $: runID, init();
</script>

<div class="container">
  <AuthSetup />
  <div style="display:flex;align-items:center;gap:1rem;margin-bottom:1rem">
    {#if run}<a href="#/jobs/{run.jobName}">← {run.jobName}</a>{/if}
    <h1>Run Detail</h1>
    {#if run && ["Running", "Queued", "Pending"].includes(run.status)}
      <button class="btn btn-danger" on:click={cancelRun}>Cancel</button>
    {/if}
    {#if run && ["Succeeded", "Failed", "Cancelled"].includes(run.status)}
      <button class="btn" on:click={rebuild}>↺ Rerun</button>
    {/if}
  </div>
  <div style="border-bottom:1px solid var(--border);margin-bottom:1.5rem">
    <a href="#/runs/{runID}" class="tab-link tab-active">Logs</a>
    <a href="#/runs/{runID}/yaml" class="tab-link">YAML</a>
  </div>
  {#if loading}<div class="loading">Loading...</div>
  {:else if error}<div class="error">{error}</div>
  {:else if run}
    <div class="card grid-2" style="margin-bottom:1rem">
      <div>
        <div class="meta">Status</div>
        <span class={statusBadge(run.status)}>{run.status}</span>
      </div>
      <div>
        <div class="meta">Triggered by</div>
        <div>{run.triggeredBy}</div>
      </div>
      <div>
        <div class="meta">Created</div>
        <div>{fmtTime(run.createdAt)}</div>
      </div>
      {#if run.params && Object.keys(run.params).length}
        <div style="grid-column:1/-1">
          <div class="meta" style="margin-bottom:0.35rem">Params</div>
          <div class="params-grid">
            {#each Object.entries(run.params) as [k, v]}
              <span class="param-k">{k}</span>
              <span class="param-v">{v}</span>
            {/each}
          </div>
        </div>
      {/if}
    </div>
    {#if steps.length}
      <h2 style="margin-bottom:0.5rem">Steps</h2>
      <div class="step-list">
        {#each groupedStages as group (group.stageIndex)}
          {#if group.steps.length > 1}
            <!-- Parallel group header -->
            <div
              class="stage-group-header {group.steps.some(s => selectedStep === s.index) ? 'active' : ''}"
              on:click={() => {
                const indices = group.steps.map(s => s.index);
                const allSelected = selectedParallelGroup !== null && indices.every(i => selectedParallelGroup.includes(i));
                selectedStep = allSelected ? null : indices[0];
                selectedParallelGroup = allSelected ? null : indices;
              }}
              role="button"
              tabindex="0"
              on:keydown={(e) => e.key === 'Enter' && (() => {
                const indices = group.steps.map(s => s.index);
                selectedParallelGroup = selectedParallelGroup?.join() === indices.join() ? null : indices;
                selectedStep = null;
              })()}
            >
              <span class="parallel-label">parallel</span>
              <span class="meta" style="font-size:0.75rem">
                {group.steps.map(s => s.name).join(' · ')}
              </span>
            </div>
            {#each group.steps as s (s.index)}
              <div
                class="step-row step-row-indented {selectedStep === s.index ? 'active' : ''}"
                on:click={() => selectStep(s.index)}
                role="button"
                tabindex="0"
                on:keydown={(e) => e.key === 'Enter' && selectStep(s.index)}
              >
                <span class={statusBadge(s.status)}>{s.status}</span>
                <span class="step-name">{s.name}</span>
                <span class="step-duration">{stepDuration(s)}</span>
                {#if s.exitCode != null}<span class="step-exit">exit {s.exitCode}</span>{/if}
              </div>
            {/each}
          {:else}
            <!-- Single sequential step -->
            <div
              class="step-row {selectedStep === group.steps[0].index ? 'active' : ''}"
              on:click={() => selectStep(group.steps[0].index)}
              role="button"
              tabindex="0"
              on:keydown={(e) => e.key === 'Enter' && selectStep(group.steps[0].index)}
            >
              <span class={statusBadge(group.steps[0].status)}>{group.steps[0].status}</span>
              <span class="step-name">{group.steps[0].name}</span>
              <span class="step-duration">{stepDuration(group.steps[0])}</span>
              {#if group.steps[0].exitCode != null}<span class="step-exit">exit {group.steps[0].exitCode}</span>{/if}
            </div>
          {/if}
        {/each}
      </div>
    {/if}
    <div class="log-header">
      <h2>Logs</h2>
      {#if selectedStep !== null}
        <span class="meta" style="font-size:0.8rem"
          >— {stepName(selectedStep)}</span
        >
        <button
          class="btn"
          style="padding:0.2rem 0.5rem;font-size:0.75rem"
          on:click={() => (selectedStep = null)}>All Steps</button
        >
      {/if}
      <span class="meta" style="font-size:0.75rem">SSE</span>
    </div>
    <div class="log-box" bind:this={logBox}>
      {#if !filteredLogs.length}
        <span style="color:var(--text-muted)">Waiting for logs…</span>
      {:else}
        {#each filteredLogs as l, i (i)}
          <div>
            {#if selectedStep === null}<span
                class="meta"
                style="font-size:0.7rem;margin-right:0.4rem"
                >{stepName(l.stepIndex)}</span
              >{/if}<span
              class={l.stream === "stderr" ? "log-stderr" : "log-stdout"}
              >{l.line}</span
            >
          </div>
        {/each}
      {/if}
    </div>
  {/if}
</div>

<style>
  .params-grid {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.2rem 0.75rem;
    font-size: 0.8rem;
    font-family: monospace;
  }
  .param-k {
    color: var(--text-muted);
    white-space: nowrap;
  }
  .param-v {
    color: var(--text);
    font-weight: 500;
    word-break: break-all;
  }
  .stage-group-header {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.3rem 0.5rem;
    background: var(--surface-alt, var(--surface));
    border-radius: 4px;
    cursor: pointer;
    margin-bottom: 0.1rem;
  }
  .stage-group-header:hover,
  .stage-group-header.active {
    background: var(--primary-light, #e8f0fe);
  }
  .parallel-label {
    font-size: 0.65rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
    border: 1px solid var(--border);
    border-radius: 3px;
    padding: 0.1rem 0.3rem;
  }
  .step-row-indented {
    margin-left: 1.2rem;
  }
</style>
