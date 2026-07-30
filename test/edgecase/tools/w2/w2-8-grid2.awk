# Emit "<ts> <pid> REAPER|DECIDE [runid]" for run_approvals UPDATEs.
# Handles both simple-protocol ("statement:") and extended-protocol
# ("execute stmtcache_...:") log forms — the reaper's arg-less query logs as the
# former, DecideApproval's parameterised query as the latter.
/^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]+ UTC \[/ {
  if (armed=="DECIDE" && $0 ~ /DETAIL: *parameters:/) {
    rid=""; if (match($0, /\$1 = '"'"'[0-9a-f-]+'"'"'/)) { rid=substr($0,RSTART+6,RLENGTH-7) }
    print cts, cpid, "DECIDE", rid; armed=""; next
  }
  cts=$1" "$2; cpid=$4; pend=($0 ~ /(statement:|execute stmtcache)/); seen=0; armed=""; next
}
pend==1 && /UPDATE run_approvals/ { seen=1; next }
pend==1 && seen==1 {
  if ($0 ~ /TimedOut/)     { print cts, cpid, "REAPER"; pend=0; seen=0; next }
  if ($0 ~ /status = \$3/) { armed="DECIDE"; pend=0; seen=0; next }
  pend=0; seen=0
}
