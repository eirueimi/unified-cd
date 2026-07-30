#!/usr/bin/env sh
# W2-4 Part B, phase-aware variant.
# Usage (from test/ha): w2-4-partB-phase.sh <target-mod30> <offset> <tag>
# The queued-reaper sweep grid on this rig sits at (epoch mod 30) ~= 29.05
# (measured: partB-sweeps.txt, every cluster at wall seconds :29.0x / :59.0x-.15).
# Triggering at a chosen (created_at mod 30) therefore fixes how much head-room
# the run has between becoming reapable (created_at + 30s) and the reaper's next
# opportunity. target-mod30 = 26.6 puts the boundary ~2.4s before a sweep, so an
# agent returning at offset > ~2.4s loses and one returning earlier wins.
set -eu
TARGET="$1"; OFFSET="$2"; TAG="$3"
SCRATCH="${SCRATCH:?run from test/ha with SCRATCH exported, e.g. export SCRATCH=<scratchpad>/w2-4}"
S="$SCRATCH"
NOW=$(date +%s.%N)
WAIT=$(awk -v n="$NOW" -v t="$TARGET" 'BEGIN{p=n-int(n/30)*30; w=t-p; if(w<0) w+=30; printf "%.3f", w}')
echo "phase_wait=$WAIT (now_mod30=$(awk -v n="$NOW" 'BEGIN{printf "%.3f", n-int(n/30)*30}'))"
sleep "$WAIT"
sh "$(dirname "$0")/w2-4-partB-trial.sh" "$OFFSET" "$TAG"
