// Shared helpers for showing agent volume usage.

const BINARY_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

// fmtBytes renders a byte count with binary units. Absent values render as a dash,
// never as zero.
export function fmtBytes(n) {
  if (n === null || n === undefined || Number.isNaN(n)) return '—';
  if (n < 1024) return `${Math.round(n)} B`;
  let v = n;
  let i = 0;
  while (v >= 1024 && i < BINARY_UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${BINARY_UNITS[i]}`;
}

const QUANTITY_FACTORS = {
  Ki: 1024, Mi: 1024 ** 2, Gi: 1024 ** 3, Ti: 1024 ** 4, Pi: 1024 ** 5, Ei: 1024 ** 6,
  k: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15, E: 1e18,
  '': 1,
};

// parseQuantity converts a Kubernetes quantity string ("200Gi", "500G", "1000000")
// to a number of bytes. It returns null when the string cannot be parsed.
export function parseQuantity(q) {
  if (typeof q !== 'string') return null;
  const m = /^\s*(\d+(?:\.\d+)?|\.\d+)\s*(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?\s*$/.exec(q);
  if (!m) return null;
  return parseFloat(m[1]) * QUANTITY_FACTORS[m[2] || ''];
}

// isK8sAgent is true for agents run by the Kubernetes agent (os ends in "/k8s").
export function isK8sAgent(agent) {
  return typeof agent?.os === 'string' && agent.os.endsWith('/k8s');
}

// isOnline applies the 90 second last-seen rule.
export function isOnline(lastSeenAt) {
  return Date.now() - new Date(lastSeenAt).getTime() < 90000;
}

// volumeRows builds one row per volumeStatus entry, joined with the agent's
// storage readings and the volume declaration. Missing values are null.
export function volumeRows(agent, declsByName) {
  return (agent?.volumeStatus || []).map((vs) => {
    const reading = agent.storage?.volumes?.[vs.name];
    const spec = declsByName?.[vs.name]?.spec;
    const size = spec?.size ?? null;
    const usedBytes = reading?.usedBytes ?? null;
    const sizeBytes = parseQuantity(size);
    return {
      name: vs.name,
      mode: spec?.mode ?? null,
      size,
      ready: vs.ready,
      reason: vs.reason ?? null,
      usedBytes,
      freeBytes: reading?.freeBytes ?? null,
      totalBytes: reading?.totalBytes ?? null,
      measuredAt: reading?.measuredAt ?? null,
      claimBlocked: reading?.claimBlocked ?? null,
      overSize: usedBytes != null && sizeBytes != null && usedBytes > sizeBytes,
    };
  });
}
