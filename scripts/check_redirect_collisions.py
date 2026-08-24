#!/usr/bin/env python3
"""Guard against redirect_maps entries that overwrite a real page.

`mkdocs build --strict` validates *source* links (that link targets exist as
docs), but it does not validate what mkdocs-redirects *writes*. The
mkdocs-redirects plugin runs in `on_post_build` and renders a stub HTML file
for every `redirect_maps` key using the same directory-URL rules MkDocs uses
for real pages. If a redirect key happens to render to the same on-disk path
as a page MkDocs also builds from docs/, the plugin's post-build write
clobbers the real page with a self-referential (or otherwise wrong) redirect
stub -- silently, because `--strict` only checks links resolvable at
"source" (build) time, not files written afterwards in a later build phase.
That is exactly the class of bug found in mkdocs.yml's
`troubleshooting.md: troubleshooting/index.md` entry: it rendered to
`site/troubleshooting/index.html`, the same path as the real
`docs/troubleshooting/index.md`, and the stub silently won.

This script reproduces mkdocs' directory-URL rendering for both (a) every
`redirect_maps` key and (b) every real page under `docs/`, and fails loudly
if any redirect-rendered path collides with a source-page-rendered path.

Usage: python scripts/check_redirect_collisions.py [path/to/mkdocs.yml]
"""
from __future__ import annotations

import pathlib
import sys

try:
    import yaml
except ImportError:
    print("error: PyYAML is required to run this check (pip install pyyaml)", file=sys.stderr)
    sys.exit(2)


def rendered_path(doc_path: str) -> str:
    """Reproduce MkDocs' use_directory_urls=true rendering of a docs-relative path."""
    doc_path = doc_path.replace("\\", "/")
    if doc_path == "index.md":
        return "index.html"
    if doc_path.endswith("/index.md"):
        return doc_path[: -len("index.md")] + "index.html"
    if doc_path.endswith(".md"):
        return doc_path[: -len(".md")] + "/index.html"
    return doc_path


def main() -> int:
    repo_root = pathlib.Path(__file__).resolve().parent.parent
    config_path = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else repo_root / "mkdocs.yml"

    with open(config_path, "r", encoding="utf-8") as f:
        # mkdocs.yml uses !!python/name and other custom tags; unsafe load
        # is what `mkdocs` itself does. We only need the plain-scalar keys
        # below, so fall back to a permissive loader with unknown tags ignored.
        class _IgnoreUnknownTagsLoader(yaml.SafeLoader):
            pass

        def _ignore_unknown(loader, tag_suffix, node):
            return None

        _IgnoreUnknownTagsLoader.add_multi_constructor("!", _ignore_unknown)
        f.seek(0)
        config = yaml.load(f, Loader=_IgnoreUnknownTagsLoader)

    docs_dir_name = config.get("docs_dir", "docs")
    docs_dir = config_path.parent / docs_dir_name

    redirect_maps = {}
    for plugin in config.get("plugins", []) or []:
        if isinstance(plugin, dict) and "redirects" in plugin:
            redirect_maps = (plugin["redirects"] or {}).get("redirect_maps", {}) or {}
            break

    if not redirect_maps:
        print("No redirect_maps entries found; nothing to check.")
        return 0

    # docs excluded from the build (e.g. vendored skill files) never get
    # rendered, so they can't collide with anything -- mirror exclude_docs
    # (simple "starts with this directory" / exact-file patterns, which is
    # all this project's config currently uses) to avoid false positives.
    exclude_raw = (config.get("exclude_docs") or "").strip().splitlines()
    exclude_prefixes = tuple(p for p in exclude_raw if p.endswith("/"))
    exclude_exact = {p for p in exclude_raw if not p.endswith("/")}

    def is_excluded(rel: str) -> bool:
        return rel in exclude_exact or any(rel.startswith(p) for p in exclude_prefixes)

    # Real pages MkDocs will render from docs/.
    source_pages = {}
    for md_file in docs_dir.rglob("*.md"):
        rel = md_file.relative_to(docs_dir).as_posix()
        if is_excluded(rel):
            continue
        source_pages[rendered_path(rel)] = rel

    collisions = []
    for key, target in redirect_maps.items():
        redirect_rendered = rendered_path(key)
        if redirect_rendered in source_pages:
            collisions.append((key, target, redirect_rendered, source_pages[redirect_rendered]))

    if collisions:
        print("REDIRECT COLLISION GUARD FAILED", file=sys.stderr)
        print(
            "The following redirect_maps entries render to the same on-disk "
            "path as a real page under docs/. mkdocs-redirects writes its "
            "stub after the real page is built, so it will silently "
            "overwrite the real page.",
            file=sys.stderr,
        )
        for key, target, rendered, real_source in collisions:
            print(
                f"  - redirect_maps entry {key!r} -> {target!r} renders to "
                f"{rendered!r}, which collides with real page {real_source!r}",
                file=sys.stderr,
            )
        return 1

    print(f"OK: checked {len(redirect_maps)} redirect_maps entries against "
          f"{len(source_pages)} source pages; no collisions.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
