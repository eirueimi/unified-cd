# Job Description in WebUI — Design

**Date:** 2026-07-15
**Status:** Approved (design review done in session)

## Problem

A Job has no human-readable description. The only `description` field in the
DSL is on individual input params (`spec.params.inputs[].description`). Job
authors want to document what a job does and have it visible in the WebUI's
job list and job detail pages.

## Decisions (from design review)

| Question | Decision |
|---|---|
| DSL placement | `spec.description` (job-level, sibling of `timeoutMinutes`/`native`/`shell`) — optional, backward-compatible |
| Rendering | **Plain text** (no Markdown this iteration; can add later) |
| Where shown | Job list (subtitle under the name) and job detail (under the heading) |

## Design

### DSL

Add `Description string` to `dsl.Spec` (`internal/dsl/types.go`):

```go
// Description is a human-readable summary of the job, shown in the WebUI.
Description string `yaml:"description,omitempty" json:"description,omitempty"`
```

Optional, so existing job YAML is unaffected. No new validation (free text);
it round-trips through the stored spec JSON like every other spec field.

### API

Add `Description string \`json:"description,omitempty"\`` to `api.Job`
(`internal/api/types.go`). Populate it in the job list and get handlers
(`internal/controller/api_jobs.go`) the same way `Inputs` is populated: parse
the stored spec JSON and copy the field. Extend the existing `specInputs`
helper into a `specMeta` that returns both inputs and description in one
parse (or add a sibling `specDescription`) — either way, one `json.Unmarshal`
of the spec per job, keeping the existing per-job cost.

Both `GET /api/v1/jobs` and `GET /api/v1/jobs/{name}` return the field.

### WebUI

- **JobList.svelte**: under the job name link, when `row.job.description` is
  non-empty, render a single-line muted subtitle (`class="meta"`, truncated
  with ellipsis on overflow). Jobs without a description render nothing extra
  — no layout shift.
- **JobDetail.svelte**: the page currently shows only `jobName` in the `<h1>`
  and fetches runs, not the job object. Add a `GET /api/v1/jobs/{name}` fetch
  to obtain the job, and render its `description` as a muted paragraph under
  the `<h1>` when present. A fetch failure degrades silently (description just
  doesn't show — the runs list is the primary content).

### Docs

- `docs/jobs.md`: document `spec.description` in the job reference (a short
  free-text summary shown in the WebUI).

## Out of scope

- Markdown / rich-text rendering of the description.
- Description on folders/namespaces, schedules, or runs.
- Search/filter by description text (the list filter stays name-based).
- A dedicated edit UI — description is authored in the job YAML like all
  other job config.

## Testing

- DSL: parse round-trip — a `spec.description` survives `dsl.Parse` and
  re-marshal; absent description → empty string, no error.
- API: `specMeta`/extraction returns the description from a stored spec;
  a spec without one yields `""`; malformed spec yields `""` without error
  (mirrors `specInputs`' lenient contract). Handler test: `GET /jobs` and
  `GET /jobs/{name}` include `description`.
- WebUI (jsdom): JobList renders the subtitle when description present and
  omits it when absent; JobDetail renders the description paragraph when the
  job fetch returns one and omits it (no crash) when the fetch fails or the
  description is empty.
