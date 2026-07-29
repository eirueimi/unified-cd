# Campaign Findings

One entry per invariant violation or notable observation. Reported as one
batch at the end of the campaign; the operator prioritizes.

Severity: **critical** (data loss / silent corruption / security),
**major** (incorrect visible behavior, unbounded recovery),
**minor** (diagnosability, docs gap, cosmetic).

Entry template:

    ## <scenario-id> — <one-line title>
    - **Invariant:** I<n> (<name>)
    - **Severity:** critical | major | minor
    - **Repro:** <commands / probe name>
    - **Observed:** <what happened, with log/query excerpts>
    - **Expected:** <what the docs/spec promise>
    - **Notes:** <fix ideas, related known issues>

---

(no findings yet)
