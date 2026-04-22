# CLAUDE.md

This repository follows a split workflow so upstream sync stays simple while local experimentation remains flexible.

## Branch Roles
- `main` is reserved for syncing with `upstream/main` and should stay close to upstream.
- Personal or self-use features should usually live on `local/self-use`, or on topic branches created from the latest `main`.
- Most self-use work stays local; if it later becomes shareable, clean it up on its branch before deciding whether to open a PR.

## Local-Only Assets
- `.trellis/` is local workflow state for this clone and should remain untracked.
- Prefer `.git/info/exclude` for ignoring local-only paths such as `.trellis/`, temporary artifacts, personal binaries, and scratch directories.
- Do not add personal-only ignore rules to shared repository files unless the project truly needs them.

## Sync Routine
1. Finish or stash branch-local work.
2. Switch to `main`.
3. Fetch and sync from `upstream/main`.
4. Switch back to your personal branch and merge or rebase from the updated `main` as needed.

```bash
git switch main
git fetch upstream
git merge upstream/main
git switch local/self-use
git merge main
```

## Documentation Promotion
- Treat `.trellis/` as private workflow support, not canonical project documentation.
- When a local convention proves broadly useful, move the stable guidance into normal repository docs such as `AGENTS.md`, `CLAUDE.md`, or `docs/`.
