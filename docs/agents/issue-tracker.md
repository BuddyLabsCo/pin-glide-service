# Issue tracker: GitHub

Issues and specs live in GitHub Issues for `BuddyLabsCo/pin-glide-service`.
Run the `gh` CLI inside this repository.

## Operations

- Create: `gh issue create --title "..." --body-file <path>`
- Read: `gh issue view <number> --comments`
- List: `gh issue list --state open --json number,title,body,labels`
- Comment: `gh issue comment <number> --body-file <path>`
- Apply or remove labels: `gh issue edit <number> --add-label "..."` or `--remove-label "..."`
- Close: `gh issue close <number> --comment "..."`

Write multiline content to a temporary file and pass it with `--body-file`.
Match the language of existing tracker items; if none exist, ask the user first.

## Pull requests as a triage surface

**PRs as a request surface: no.**

## Skill terminology

- "Publish to the issue tracker": create a GitHub issue.
- "Fetch the relevant ticket": run `gh issue view <number> --comments`.
