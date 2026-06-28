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
