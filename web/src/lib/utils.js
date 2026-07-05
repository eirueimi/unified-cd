export function statusBadge(status) {
  const map = { Succeeded: 'badge-success', Running: 'badge-running', Failed: 'badge-failed', Pending: 'badge-pending', Queued: 'badge-queued', Cancelled: 'badge-cancelled' };
  return 'badge ' + (map[status] || 'badge-pending');
}
export function fmtTime(ts) {
  if (!ts) return '';
  return new Date(ts).toLocaleString();
}
export function fmtRelative(ts) {
  if (!ts) return '';
  const diff = Date.now() - new Date(ts).getTime();
  const s = Math.floor(diff / 1000);
  if (s < 60) return s + 's ago';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}
export function matchesFilter(name, query) {
  if (!query) return true;
  return name.toLowerCase().includes(query.toLowerCase());
}

// buildJobTree groups a flat job list into a nested folder tree keyed by the
// job's `path`. Root-level jobs (empty path) attach to the returned root node.
export function buildJobTree(jobs) {
  const root = { name: '', path: '', folders: new Map(), jobs: [] };
  for (const j of jobs) {
    const segs = j.path ? j.path.split('/') : [];
    let node = root, acc = '';
    for (const seg of segs) {
      acc = acc ? acc + '/' + seg : seg;
      if (!node.folders.has(seg)) node.folders.set(seg, { name: seg, path: acc, folders: new Map(), jobs: [] });
      node = node.folders.get(seg);
    }
    node.jobs.push(j);
  }
  return root;
}

// flattenJobTree produces ordered display rows. Folders come before their
// sibling jobs, both sorted by name. A folder is open when the query is
// non-empty (search auto-expands) or its path is not in `collapsed`. With a
// query, only matching jobs and their ancestor folders are emitted.
export function flattenJobTree(root, collapsed, query) {
  const rows = [];
  const q = (query || '').toLowerCase();
  const jobMatches = (j) => !q || j.name.toLowerCase().includes(q);
  const folderHasMatch = (node) =>
    node.jobs.some(jobMatches) || [...node.folders.values()].some(folderHasMatch);
  function walk(node, depth) {
    for (const name of [...node.folders.keys()].sort()) {
      const f = node.folders.get(name);
      if (q && !folderHasMatch(f)) continue;
      rows.push({ kind: 'folder', name, path: f.path, depth });
      const open = q ? true : !collapsed.has(f.path);
      if (open) walk(f, depth + 1);
    }
    for (const j of [...node.jobs].filter(jobMatches).sort((a, b) => a.name.localeCompare(b.name))) {
      rows.push({ kind: 'job', job: j, depth });
    }
  }
  walk(root, 0);
  return rows;
}
