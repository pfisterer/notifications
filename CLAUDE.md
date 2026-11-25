# notifications — notes for Claude Code

Read `README.md` first. This file lists the rules that are easy to get wrong.

- The event contract (`POST /v1/events`, README "The contract") is used by other services (openstack-management-api first). Change it only compatibly; a new field is fine, a changed meaning is not.
- Every rule against flooding people (bundling, daily limit, reminders, own actions, duplicates) has a test in `internal/engine/engine_test.go`; a rule change comes with its test.
- A new event type with its own text goes into `internal/texts/texts.go` in German and English (`texts_test.go` checks both) and into the README table.
- German texts: neutral like the portal, "Sie" where an address cannot be avoided.
- One replica only (no coordination between delivery loops).

## Conventions

- Commits: `area: lowercase summary`, no co-author or other attribution lines.
- Comments explain why; no hard-wrapped comments, strings or Markdown.
