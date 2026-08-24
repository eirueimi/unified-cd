# Documentation Site on GitHub Pages — Design

Date: 2026-08-20
Status: Approved (design); implementation plan to follow

## 1. Purpose

`docs/` holds 17 Markdown files and roughly 10,700 lines of reference-grade
material, but it is published only as a flat directory of files on GitHub with
`README.md` acting as the index. Two different audiences — people writing Jobs
and people operating a controller — read the same flat list, and single files
have grown past 2,000 lines.

Publish the existing documentation as a navigable static site on GitHub Pages,
reorganized into an audience-oriented tree, with the largest files split into
topic pages.

The goal is findability and ease of onboarding, not visual imitation of any
particular project's site.

## 2. Scope

In scope:

- A MkDocs + Material site built from `docs/`, published to GitHub Pages.
- A new information architecture (section 4) expressed as `nav:` in `mkdocs.yml`.
- Splitting `jobs.md`, `troubleshooting.md`, and `resources.md` into topic pages.
- Moving every remaining `docs/*.md` file into its section directory.
- Repairing every link affected by those moves, repo-wide.
- CI that builds the site on pull requests and deploys it from `main`.

Out of scope (deliberately deferred, and the design keeps the door open for
each):

- A Japanese translation. English only for now; `mkdocs-static-i18n` can be
  added later against a `docs/ja/` overlay.
- Per-release versioned documentation (`mike`).
- A marketing landing page. `docs/index.md` is a short entry page, not a
  product page.
- Rewriting the prose of any existing page. This project moves, splits, and
  relinks existing text. Newly authored prose is limited to `docs/index.md`,
  the troubleshooting symptom index, and a one-line generated-file notice on
  the field reference — all assembled from material that already exists.

## 3. Tooling decision

**MkDocs with the Material theme.**

Rationale:

- Input stays plain Markdown. No front matter is required, so the existing
  17 files move without per-file edits beyond link repair.
- Navigation lives in `mkdocs.yml`, decoupled from file layout. The tree can be
  restructured independently of where files sit, which makes the reorganization
  reviewable in stages.
- Sidebar navigation, per-page table of contents, offline full-text search,
  code-copy buttons, admonitions, dark mode, and responsive layout are all
  theme defaults. No plugin selection is needed to reach a readable site.
- `strict: true` turns every broken internal link into a build failure, which
  is the safety net the link migration in section 6 depends on.

Rejected alternatives:

- **Docusaurus.** The repository already uses npm for the Svelte web UI, so it
  adds no new language runtime. Rejected because it requires per-file front
  matter, a `sidebars.js`, and a separate search plugin (local search or an
  Algolia account), and because sharing a dependency tree with the web UI means
  documentation changes can touch the UI lockfile.
- **Hugo (Docsy or similar).** A single Go binary matches the repository's
  character and builds fastest. Rejected because Docsy pulls in npm anyway for
  its asset pipeline, and its theme configuration surface is larger than
  Material's for the same result.

The cost accepted with MkDocs is a Python toolchain. It is confined to CI and
to optional local preview; no Go or web-UI build path depends on it.

## 4. Information architecture

```
Home                                    docs/index.md
Getting Started
  Installation                          getting-started/installation.md
  Quickstart                            getting-started/quickstart.md
  Core Concepts                         getting-started/concepts.md
User Guide
  Writing Jobs                          user-guide/writing-jobs/*.md   (10 pages)
  Resources                             user-guide/resources/*.md      (6 pages)
  Secrets                               user-guide/secrets.md
Operator Manual
  Agents and Routing                    operator-manual/agents.md
  Kubernetes Integration                operator-manual/kubernetes-integration.md
  Authentication                        operator-manual/authentication.md
  Authorization                         operator-manual/authorization.md
  High Availability                     operator-manual/high-availability.md
  Operations and Backup                 operator-manual/operations.md
  Audit Log                             operator-manual/audit.md
  Migrations                            operator-manual/migrations/*.md
Reference
  CLI Reference                         reference/cli.md
  Configuration Reference               reference/configuration.md
  Field Reference (generated)           reference/field-reference.md
  JSON Schema and Editor Setup          reference/json-schema.md
Troubleshooting
  Symptom Index                         troubleshooting/index.md
  (7 area pages)                        troubleshooting/*.md
Contributing
  Contributing                          contributing/index.md
  Frontend Development                  contributing/frontend-development.md
```

The split that matters is between documents that are read in order (Getting
Started, User Guide) and documents that are consulted (Reference), and between
the person writing Jobs and the person operating the deployment.

`cli.md` and `configuration.md` appear **once**, under Reference. An earlier
draft placed a task-oriented CLI page under User Guide and the full flag list
under Reference; that is rejected because it would require authoring new prose
and would create two places where the same command is documented.

## 5. File reorganization

### 5.1 Split: `jobs.md` (2,001 lines) into ten pages

| New page (`user-guide/writing-jobs/`) | Source H2 sections |
|---|---|
| `job-structure.md` | Job Structure, Metadata, Job Description, Job-level Timeout |
| `parameters.md` | Parameters (inputs / outputs) |
| `steps.md` | Steps (run, shell, needs, if, env, outputs, timeout, continue-on-error, retry, post) |
| `expressions.md` | Template Syntax, Status Functions in `if:` |
| `artifacts-and-cache.md` | Artifacts, Cache |
| `templates-and-reuse.md` | Git Template Inlining (`uses`), Uses-level `runsIn.image`, Calling Other Jobs (`call`) |
| `isolation-and-containers.md` | Job Isolation (`native` and the claim pod), Kubernetes Pod Template |
| `concurrency-and-agent-selection.md` | Concurrency Control, Agent Selection |
| `approval-and-finally.md` | Approval Step, Finally Block |
| `complete-example.md` | Complete Example |

The `Secrets in Jobs` section is merged into `user-guide/secrets.md` rather than
becoming its own page, removing an existing duplication between the two files.

### 5.2 Split: `troubleshooting.md` (1,328 lines, 34 symptom sections)

Thirty-four pages would be worse than one. The symptoms are grouped into seven
area pages — Runs and Scheduling, Steps and Execution, Artifacts and Storage,
Templates and `uses`, Webhooks, Agents and Enrollment, and Controller and
Database — each keeping its symptom headings verbatim.

`troubleshooting/index.md` lists all 34 symptoms, one line each, linked to the
heading that covers it. This preserves the "search by the error message you
saw" entry path, which is the page's primary use.

### 5.3 Split: `resources.md` (828 lines) into six pages

One page per kind: `job.md`, `job-template.md`, `schedule.md`,
`webhook-receiver.md`, `git-credential.md`, `app-source.md`. The navigation
list of kinds then serves as the index.

### 5.4 Not split

`cli.md`, `configuration.md`, and `field-reference.md` remain single pages.
They are consulted with in-page search and the right-hand table of contents;
splitting a flag list makes it slower to search, not faster.
`field-reference.md` gains a leading note that it is generated by
`cmd/docgen` and must not be hand-edited.

### 5.5 Moved without change of content

`getting-started.md` becomes `getting-started/quickstart.md`; `agents.md`,
`kubernetes-integration.md`, `authentication.md`, `authorization.md`,
`high-availability.md`, `operations.md`, and `audit.md` move under
`operator-manual/`; `migration-agent-id-scoped-credentials.md` moves to
`operator-manual/migrations/`; `frontend-development.md` moves under
`contributing/`.

`getting-started/installation.md` and `getting-started/concepts.md` are formed
by moving the Installation and Architecture sections out of `README.md`.
`README.md` keeps a short Docker pull snippet, the architecture summary
paragraph, and links to the site. `reference/json-schema.md` is formed from the
editor-integration section at the end of `getting-started.md` together with the
pointer to `editors/vscode/README.md`.

`contributing/index.md` is `CONTRIBUTING.md` surfaced on the site.
`CONTRIBUTING.md` remains at the repository root, because GitHub's contribution
UI looks for it there; the site page includes it via `pymdownx.snippets` rather
than duplicating it.

### 5.6 Generator change

`cmd/docgen/main.go:66` writes to `docs/field-reference.md`. Moving the file to
`docs/reference/field-reference.md` requires updating that path, the package
doc comment on line 1, and the generated-artifacts note in `AGENTS.md`. Run
`go generate ./...` afterwards and confirm the output lands in the new location
and the diff is empty otherwise.

### 5.7 Excluded from the site

`docs/superpowers/plans/` and `docs/superpowers/specs/` are internal planning
artifacts. They stay where they are and are excluded with MkDocs 1.6's
`exclude_docs:`, which needs no plugin and no file movement.

## 6. Link migration

Four categories of references break when files move. All four are repaired in
the same change that moves the files.

1. **Cross-links and anchor links between `docs/*.md`** — several dozen,
   including anchor links such as `jobs.md#job-isolation-native-and-the-claim-pod`
   whose target heading moves to a different page.
2. **The documentation index in `README.md`** — about 20 links, rewritten to
   point at the published site.
3. **Relative links that escape `docs/`** — 7 occurrences such as
   `../internal/dsl/container.go` and
   `../deployments/observability/prometheus-alerts.yaml`. These cannot resolve
   in a static site and are rewritten as absolute
   `https://github.com/eirueimi/unified-cd/blob/main/...` URLs.
4. **References inside Go comments and `examples/*.yaml` comments** — about 26
   occurrences such as `docs/jobs.md#shell-shell`. These do not affect any
   build, but they become false statements once the file is gone, so they are
   updated in the same change.

Mitigations:

- `mkdocs-redirects` maps every old site path to its new one, so links to the
  published site keep working.
- `strict: true` plus `mkdocs build --strict` in pull-request CI makes any
  unrepaired internal link a red build.

Accepted degradation: URLs of the form
`https://github.com/eirueimi/unified-cd/blob/main/docs/jobs.md` will 404,
because the file genuinely stops existing in the repository and no redirect
mechanism covers GitHub blob paths. Leaving stub files behind was considered
and rejected — the stubs would themselves need maintenance, and the site
becomes the canonical location. The project is early enough that the number of
external bookmarks is small.

## 7. Build and publish

New files:

```
mkdocs.yml                      site definition, including nav
docs/requirements.txt           mkdocs, mkdocs-material, mkdocs-redirects (pinned)
docs/index.md                   Home
.github/workflows/docs.yml      build on PR, deploy from main
```

`.gitignore` gains `/site/`. The `Makefile` gains `docs-serve` and `docs-build`.

`mkdocs.yml` outline:

```yaml
site_name: unified-cd
site_url: https://eirueimi.github.io/unified-cd/
repo_url: https://github.com/eirueimi/unified-cd
edit_uri: edit/main/docs/
strict: true
exclude_docs: |
  superpowers/
theme:
  name: material
  language: en
  features:
    - navigation.sections
    - navigation.top
    - navigation.tracking
    - toc.follow
    - content.code.copy
    - search.suggest
    - search.highlight
  palette: [light/dark toggle]
plugins:
  - search
  - redirects: {redirect_maps: <old path -> new path, section 6>}
markdown_extensions:
  - admonition
  - pymdownx.details
  - pymdownx.superfences
  - pymdownx.tabbed
  - pymdownx.highlight
  - pymdownx.snippets
  - attr_list
  - toc: {permalink: true}
nav: [the tree in section 4]
```

Dependency versions in `docs/requirements.txt` are pinned exactly so that a
site build is reproducible and an upstream theme release cannot change the
published output without a commit.

`.github/workflows/docs.yml`:

- Triggered on changes to `docs/**`, `mkdocs.yml`, and the workflow itself, so
  it never lengthens the existing CI matrix for ordinary code changes.
- On pull requests: `mkdocs build --strict` only. No deployment.
- On push to `main`: the same build, then `actions/upload-pages-artifact` and
  `actions/deploy-pages`, with `permissions: {contents: read, pages: write,
  id-token: write}` and `concurrency: {group: pages, cancel-in-progress: false}`.

Published at `https://eirueimi.github.io/unified-cd/`.

**One-time manual step, required from a repository administrator:** set
Settings → Pages → Source to **GitHub Actions**. The deploy job fails until
this is done. It cannot be performed from the repository contents.

## 8. Local development

```
pip install -r docs/requirements.txt
mkdocs serve        # or: make docs-serve
```

Serves with live reload on `http://127.0.0.1:8000`. Python is a dependency only
for people editing documentation; `make build`, `go test`, and the web-UI build
are unaffected.

While splitting files, the hand-written `## Table of Contents` sections at the
top of `jobs.md`, `cli.md`, `resources.md`, and `configuration.md` are removed —
Material renders a per-page table of contents automatically, and the manual
lists would immediately drift.

## 9. Verification

The change is complete when:

1. `mkdocs build --strict` passes with no warnings, from a clean checkout.
2. Every heading that had an inbound anchor link before the split is reachable
   from an equivalent link afterwards.
3. `go generate ./...` regenerates `docs/reference/field-reference.md` in place
   with an otherwise empty diff.
4. A repo-wide grep for `docs/<name>.md` paths in Go and YAML sources returns
   no path that no longer exists.
5. The site is reachable at the published URL, its search returns results, and
   the navigation tree matches section 4.

## 10. Staging

The work is sequenced so that a usable site exists after the first stage and
each later stage is independently reviewable:

1. **Site skeleton.** `mkdocs.yml`, `requirements.txt`, `docs/index.md`, CI,
   Makefile targets, `exclude_docs`. Files stay where they are; `nav` points at
   current paths. The site is live and buildable at the end of this stage.
2. **Move without splitting.** Files move into their section directories,
   including the `docgen` output-path change. Links repaired, redirects added.
3. **Split `jobs.md`.**
4. **Split `troubleshooting.md` and `resources.md`.**
5. **README and comment references.** `README.md` index rewritten; Go and
   example-YAML comment references updated.
