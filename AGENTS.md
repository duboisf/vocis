# Repo Rules

- Do not report work as done until the requested behavior has been verified locally.
- Keep going until the feature works end to end, or until a real external blocker is identified in logs or test output.
- When code changes affect behavior described in `docs/`, update the relevant docs in the same change.

## Repo Context (Progressive Disclosure)

Docs are organized by depth. Read only as far as you need:

1. `docs/overview.md` — what the product does and key constraints
2. `docs/architecture.md` — which packages own which behavior
3. `docs/runtime-flow.md` — detailed execution path for a dictation session

Start at the top. Stop when you have enough context for the task at hand.
