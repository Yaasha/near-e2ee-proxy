# Issue tracker: GitHub

Issues and specs live in GitHub Issues for `Yaasha/near-e2ee-proxy`.
Use the `gh` CLI, explicitly targeting this repository with
`--repo Yaasha/near-e2ee-proxy`.

## Conventions

- Before the first issue creation in a session, verify the target with
  `gh repo view --json nameWithOwner`.
- Create issues with `gh issue create --title "..." --body-file <path>`.
  Use a temporary file for multiline bodies.
- Read issues with `gh issue view <number> --json number,title,body,labels,comments`.
- List issues with `gh issue list --state open --json number,title,body,labels`.
  Apply label and state filters as needed.
- Comment with `gh issue comment <number> --body-file <path>`.
- Apply or remove labels with `gh issue edit <number> --add-label "..."`
  or `--remove-label "..."`.
- Close by posting the explanation as a comment, then running
  `gh issue close <number>`. Post explanations even if already closed.

## Relationships

Use native GitHub sub-issues and issue dependencies when supported.
Check the installed CLI's help before choosing command flags.

If sub-issues are unavailable, put `Part of #<parent>` at the top of
each child and list children in the parent's task list.

If dependencies are unavailable, put `Blocked by: #<n>, #<n>` at the
top of the dependent issue. It is startable when all blockers are closed.

## Pull requests as a triage surface

**PRs as a request surface: no.**

## Skill operations

When a skill says "publish to the issue tracker", create a GitHub issue.
When it says "fetch the relevant ticket", read the referenced issue.

## Wayfinding operations

- Map: one issue labelled `wayfinder:map`, containing Notes,
  Decisions-so-far and Fog.
- Child: an issue linked to the map, labelled `wayfinder:<type>`,
  where type is research, prototype, grilling or task.
- Frontier: the first open child in map order with no open blockers
  and no assignee.
- Claim: assign the issue to the driving developer before work.
- Resolve: comment with the answer, close the child, and append a
  concise finding and link to the map's Decisions-so-far.
