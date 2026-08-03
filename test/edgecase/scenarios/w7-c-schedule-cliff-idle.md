# W7-C — the promotion-free idle window on a `* * * * *` schedule

**Follow-up Arm C.** Discharges campaign carry-forward **2**
(`FINDINGS.md:3102`), which calls it *"STILL the campaign's best
cost-to-consequence ratio"*, and executes the boxed measurement at
`FINDINGS.md:1010` verbatim:

> apply a fresh `* * * * *` schedule to an otherwise idle stack, **inject
> nothing and kill nothing**, and sample `schedules.last_fired_at`, `NOW()` and
> `count(*) FROM runs WHERE triggered_by='schedule:...'` every 30 s. If the fire
> count freezes while `last_fired_at` keeps advancing, the cliff is reached with
> **zero** faults.

with the two validity conditions that box calls non-negotiable: the window must
be **promotion-free**, and the sampler must be **continuous**.

---

## The mechanism, restated from the code so the read-out is interpretable

`checkAndFireSchedules` (`internal/controller/scheduler.go:86-204`) sets
`windowStart = now - 1h` and `base = last_fired_at` (or `windowStart` when
`last_fired_at` is `NULL`), then takes one of three branches on
`next = NextCronTime(cron, base)`:

| branch | condition | effect |
|---|---|---|
| `:104` | `next > now` | nothing |
| `:106` | `windowStart <= next <= now` | **fire** — `CreateRun`, then `UpdateScheduleLastFiredAt` |
| `:197` (`default`) | `next < windowStart` | **advance `last_fired_at` and create nothing**, with **no log line of any kind** |

A check retires exactly one 60 s occurrence while one minute of wall clock adds
one, so for a cron period `<= 1 minute` the lag `A` obeys
`A_{k+1} = A_k + (c - 60)` where `c` is the interval between checks. The gate at
`:71` is `t.Sub(lastScheduleCheck) >= time.Minute` on a 200 ms ticker, so under
stable leadership `c >= 60` and **`A` is monotonically non-decreasing — there is
no recovery term.** Once `A > 3600` the `default` branch is taken forever.

**`lastScheduleCheck` is process-local (`:31`)**, so a freshly promoted leader
checks on its first tick, `c ~= 0` for that one interval, and the lag drops by
~60 s. That is why the window must be promotion-free: **a promotion heals it.**

---

## Prediction, recorded before the schedule was created

`controller2` logged `scheduler became leader` at **11:49:57.052**. Since
`lastScheduleCheck` is zero-initialised, the check phase is `f ~= 57.05 s` and
the birth headroom is `60 - f ~= 2.9 s`. Written into `PREDICTIONS.md` before
the schedule existed:

- **C0** birth headroom **~2.9 s** — *and this is a low draw from a
  distribution that is uniform on (0, 60]; a high draw would need ~12 h, which
  is disclosed here rather than after the fact.*
- **C1** at the campaign's measured `+0.084 s/min` the cliff arrives after
  `2.9 / 0.084 ~= 35 minutes` and **34-35 fires**, with nothing injected.
- **C2** afterwards the fire count freezes permanently, `last_fired_at` keeps
  advancing so the row looks healthy, and the controllers log nothing.
- **C3** promotion-free, checked by `docker compose ps` and a `became leader`
  grep over the whole window.
- **Band** *"predict: it lands, and the arm produces the campaign's first
  critical."*

## Delta

**C0 landed to 0.2 s (2.694 measured against 2.9 predicted).** **C2 and C3
landed exactly.** **C1 landed in kind and was 26 % fast in time**: the cliff
came after **25 fires and ~25.5 minutes**, not 34-35 and ~35, because the drift
was **+0.109 s/min** rather than the `+0.084` the prediction inherited from W2-6.
**The band prediction was WRONG, and the reason is the interesting part** — see
§Band.

---

## Rig and window

Plain `test/ha`, no fault overlays, on its own project so the two short arms
could come and go without touching it:

```bash
HA_LB_PORT=18090 docker compose -p edge-armc \
  -f docker-compose.ha.yaml \
  -f ../edgecase/compose/altports.override.yaml up -d --build
```

`controller1` lost the bootstrap-PAT race on the cold `up` and was recovered
with `docker compose up -d controller1` **before** the window opened; that
recovery is the only container action taken in the whole session, and it
predates the schedule by 2 min 31 s.

- Job `edge-tick` applied `11:52:28.187`, schedule `edge-every-minute`
  (`cron: "* * * * *"`) created `11:52:28.248`, `last_fired_at` `NULL`.
- Sampler: 30 s grid, DB clock, one `psql` per sample, **self-bounded by a
  duration argument** and terminating with its own `SAMPLER-END` line — so it
  needs no `-window.txt` sidecar and cannot become the unstoppable-capture
  defect W6 fixed twice (`README.md` §"the rule that now covers all SIX
  stop-class instrument defects").

**Promotion-free, verified two ways.** `grep -c "became leader"` over all three
controllers for the entire session returns **1**, and it is
`controller2 … 11:49:57.052` — the promotion that predates the schedule. Every
sample also carries `count(*) FROM pg_locks WHERE locktype='advisory' AND
granted`, which reads **1** in every row. No container was restarted: the final
`docker compose ps` shows uptimes consistent with one `up`.

---

## Result

### The 25 fires, and the headroom running out

`lag = created_at - occurrence`; `headroom = 3600 - lag`; `c` = interval between
consecutive fires.

| n | fired_at | occurrence | lag_s | headroom_s | c_s |
|---|---|---|---|---|---|
| 1 | 11:52:57.305 | 10:53:00 | 3597.306 | **2.694** | — |
| 2 | 11:53:57.525 | 10:54:00 | 3597.526 | 2.474 | 60.220 |
| 5 | 11:56:57.982 | 10:57:00 | 3597.982 | 2.018 | 60.215 |
| 10 | 12:01:58.677 | 11:02:00 | 3598.678 | 1.322 | 60.222 |
| 15 | 12:06:59.162 | 11:07:00 | 3599.163 | 0.837 | 60.222 |
| 20 | 12:11:59.659 | 11:12:00 | 3599.660 | 0.340 | 60.221 |
| 24 | 12:15:59.913 | 11:16:00 | 3599.914 | 0.086 | 60.209 |
| 25 | 12:16:59.928 | 11:17:00 | 3599.928 | **0.072** | 60.015 |

(The full 25-row table is `w7/w7-c/fires-midwindow.txt`; the rows above are a
sample of it and every number in this runbook is taken from the full table.)

`c` alternates between ~60.016 and ~60.221 — never below 60, exactly as the
`:71` gate guarantees — so the lag rose monotonically from 3597.306 to 3599.928
across 24 intervals: **+2.622 s over 24 checks = +0.10925 s per minute.**

### The cliff

The check after fire 25 fell at `~12:18:00.0`, which puts
`windowStart = 11:18:00.0` at or just past the occurrence `11:18:00` it was
computing — so `next.Before(windowStart)` became true and the `default` branch
at `:197` was taken. The sampler shows it directly:

```
12:17:26  last_fired_at 11:17:00   fires 25
12:17:56  last_fired_at 11:17:00   fires 25
12:18:27  last_fired_at 11:18:00   fires 25    <- advanced, no run
12:18:58  last_fired_at 11:18:00   fires 25
12:19:29  last_fired_at 11:19:00   fires 25
...
```

**`last_fired_at` advances one minute per check and `fires` does not move
again.** The schedule was **25 min 32 s old** and **nothing had been injected,
killed, paused, throttled or partitioned at any point.**

### How long the silence was observed for — stated as a window, never as "never"

The sampler ran to its own duration bound and wrote `SAMPLER-END` at
**`15:02:59Z`**, giving a total window of **3 h 10 min 05 s** from the first
sample at `11:52:54`. Measuring from the last fire at `12:16:59.928`, the
observed silence is **2 h 46 min 00 s**, and the final read at `15:03:19` puts
`last_fired_at` at `14:03:00` — i.e. **166 occurrences advanced with zero runs
created**. The claim is exactly that and not more: *0 fires in the 166
occurrences observed after the cliff*, on a window this scenario ended itself.

Three product-side corroborations from the same 3 h 10 m of container logs:

- **`checkAndFireSchedules` appears 0 times.** The `default` branch logs only
  when the `UPDATE` fails, and it never failed.
- **`"scheduler enqueued"` appears exactly 25 times** — one per fire, none
  after. The scheduler kept running its 200 ms loop and its per-minute check
  for another two and three-quarter hours and produced nothing to enqueue.
- **The whole session's `WARN`/`ERROR` count across all three controllers is
  4**, and all four are timestamped inside the first minute: three
  `key file … is readable by group or others` and the one bootstrap-PAT
  `ERROR`. **Nothing was logged at, near, or after the transition.**

### What an operator sees

At `12:20`, two minutes past the cliff:

```json
[{"name":"edge-every-minute","cron":"* * * * *","jobName":"edge-tick",
  "lastFiredAt":"2026-08-03T11:20:00Z","updatedAt":"2026-08-03T12:20:00.367Z"}]
```

and at `15:03:19`, two hours and forty-six minutes past it:

```json
[{"name":"edge-every-minute","cron":"* * * * *","jobName":"edge-tick",
  "lastFiredAt":"2026-08-03T14:03:00Z","updatedAt":"2026-08-03T15:03:19.502Z"}]
```

`lastFiredAt` is fresh and advancing in both; `updatedAt` is seconds old in
both. **The row looks healthier than it did while the schedule was working**,
because the `UPDATE` now runs on every check instead of only on a fire. All 25
runs that did happen are `Succeeded`. **There is no log line, no metric and no
API field that marks the transition** — a monitor watching this row, or
alerting on `updatedAt` staleness, sees a perfectly healthy schedule.

### The host-load confound, measured rather than assumed

W7-A ran on a second compose project from `11:58:06` to `12:02:21` and W7-B on a
third from `12:05:14` to `12:09:41`, both on this host. Host load can only
lengthen the 200 ms ticker and therefore **increase** the drift, so the arm
splits its own drift rate by window:

| window | intervals | drift |
|---|---|---|
| before the other arms (fires 1-6) | 5 | **+0.178 s/min** |
| while A and B were running (fires 6-17) | 11 | **+0.0909 s/min** |
| after both were torn down (fires 17-25) | 8 | **+0.0914 s/min** |

**The loaded window and the clean window agree to 0.5 %, and the *unloaded*
opening window is the outlier on the high side** — so the concurrency did not
inflate the drift, and the cliff was not brought forward by it. (The opening
window's excess is the `60.22`/`60.02` alternation happening to start on four
consecutive `60.22`s; over the whole run the alternation is even.)

---

## Band

`FINDINGS.md:1008` promises *"If that holds, this is critical"* — **and it is
not filed as one.** The reasoning is set out in the W7-C entry in `FINDINGS.md`
and turns on two things: the same Severity line **also** says, three sentences
earlier, that *"what it settles is reachability with zero faults, not this
entry's severity"*, so the entry contradicts itself on exactly this point; and
the campaign's most-enforced rule is that a band is argued from `:6-8`'s own
text rather than inherited from a conditional written before the measurement.
On that text nothing is lost that existed, nothing stored is damaged, and the
one route back — `DELETE` + `POST` — is through the API. **The premise is
confirmed and the band is unchanged at major.** What the arm does establish is
that the reachability is **six times cheaper than the entry's own derived
figure**: ~26 minutes, not ~156.

## Findings

| # | FINDINGS.md | Kind | Severity | Subject |
|---|---|---|---|---|
| 1 | W7-C entry | observation | **minor (observation)** | the no-fault path to the cliff is confirmed at ~25.5 minutes and 25 fires on a promotion-free idle stack; `:1005`'s premise holds, its conditional promise of `critical` does not survive `:6-8`, and its self-contradiction on that point is named and resolved |

## What this scenario does NOT establish

- **The birth headroom was a LOW draw and that is disclosed, not buried.**
  2.694 s of a distribution uniform on (0, 60]; a draw of 55 s at the same drift
  needs ~8.4 hours. **The time-to-cliff measured here is one sample and must not
  be quoted as a typical figure**; what is general is the *mechanism* and the
  monotonicity, both of which the 24 intervals establish.
- **The window was ended by the sampler's own duration argument**, so the
  correct statement is *"0 fires across the 166 occurrences observed after the
  cliff"* and never *"never fired again"*. That the state is a fixed point is a
  **code reading** (`:197-201` has no branch back) corroborated by 2 h 46 min of
  measurement, not a measurement of forever.
- **The harness reported the sampler `killed` at ~13:00 and it was still
  running** — the file kept gaining samples and later wrote its own
  `SAMPLER-END`. Every wait in this scenario therefore polls for that line
  rather than trusting the notification. **This is W6's stop-class rule
  inverted** (`README.md`: *"the process looks finished is never evidence that
  it is"*) — here the report of death was the wrong one, and the same remedy
  applies: believe the capture's own end marker, not the supervisor.
- **No promotion was tested inside the window** — deliberately, since a
  promotion heals the lag (`:1028`). The heal was measured by W2-6 and is not
  re-measured here.
- **Nothing here re-measures W2-6's Part D** (the one refused `last_fired_at`
  write), which remains the demonstrated path and is unaffected.
