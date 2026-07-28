# System kickoff status

## Current state

Not started. The repository contains the durable project instructions and this
rough kickoff plan, but no application, deployment, schema, or sub-repository
implementation yet.

## Checklist

- [ ] Establish source and contract layout.
- [ ] Define persistence and migrations.
- [ ] Implement ingestion and lifecycle processing.
- [ ] Implement the ConnectRPC API.
- [ ] Implement the web GUI.
- [ ] Build deployment composition.
- [ ] Verify the system in a KVM guest.

## Done

- Captured the system purpose, durable architecture constraints, technology
  choices, and delivery rules in
  `.apm/instructions/base.local.instructions.md`.
- Drafted the initial implementation plan and recorded unresolved decisions.
- Selected ParadeDB `pg_search` for full-text search (D-012), rejecting
  PGroonga, pg_bigm, and Elasticsearch; Japanese query-quality validation with
  real documents is a recorded follow-up.

## In progress

- Resolving the open questions in `PLAN.md`.

## Blocked

- Implementation should not begin until the open questions that affect storage,
  process boundaries, deployment privileges, and security are resolved.

## Next action

Resolve open questions 1 through 4 with the user, update `PLAN.md` and
`DECISION.md`, then resolve questions 5 and 6.
