# MAQUINISTA.md — repository review criteria

Per-repository review criteria for the pipeline reviewer. Dispatch loads
this file from the repo root when building every review-round prompt and
injects it as **binding** criteria: the reviewer must apply them on top of
its soul contract and `AGENTS.md`, and they constrain the verdict — they
are not optional guidance.

## Protected contracts: DOMAIN.md / ARCH.md

Any PR whose diff touches `DOMAIN.md` or `ARCH.md` requires human review
and approval. These files are this repository's domain and architecture
contracts; the automated reviewer must not approve such a change on its own
authority.

- If the diff touches `DOMAIN.md` or `ARCH.md` and the PR carries no
  explicit human approval (a human comment approving the change — surfaced
  to the reviewer as PR-comment input), the reviewer must escalate:
  `VERDICT: needs_human` when the change looks intentional and well-made
  and only lacks the human pass, `VERDICT: request_changes` when it also
  has substantive problems. Never `VERDICT: approve`.
- A visible human approval satisfies the requirement: judge the rest of
  the diff normally and say so in the findings.
- Creating, renaming, deleting, or restructuring either file counts as
  touching it.
