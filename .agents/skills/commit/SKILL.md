---
name: commit
description: Create a git commit with a well-crafted message. Use when the user asks to commit changes.
allowed-tools: Bash(git *)
---

# Git Commit Skill

## Rules

1. **Language**: All commit messages MUST be written in English, regardless of the language the user communicates in.

2. **No AI attribution**: NEVER include any reference to Claude, AI, or any automated tool as author, co-author, or in any form. Specifically:
   - Do NOT add `Co-Authored-By` lines mentioning Claude, Anthropic, AI, or any bot
   - Do NOT add `Signed-off-by` lines mentioning Claude or AI
   - Do NOT mention Claude, AI assistance, or automated generation anywhere in the commit message
   - The commit must appear as if it was entirely written by the human developer

3. **Message format**: Use the conventional commits style:
   - First line: short summary in imperative mood, max 72 characters (e.g., `fix: resolve login timeout on slow connections`)
   - Blank line after the subject
   - Optional body: explain the "why" behind the change, not the "what" (the diff already shows that)
   - Wrap body lines at 72 characters

4. **Commit type prefixes** (use when appropriate):
   - `feat:` - new feature
   - `fix:` - bug fix
   - `refactor:` - code restructuring without behavior change
   - `docs:` - documentation only
   - `style:` - formatting, whitespace, etc.
   - `test:` - adding or updating tests
   - `chore:` - maintenance tasks, dependencies, configs
   - `perf:` - performance improvements

## Process

1. Run `git status` and `git diff --staged` to understand staged changes. If nothing is staged, run `git diff` to see unstaged changes.
2. Run `git log --oneline -5` to see recent commit style for consistency.
3. Analyze the changes and draft an appropriate commit message.
4. Stage relevant files individually (avoid `git add -A` or `git add .`).
5. Create the commit using a HEREDOC for the message.
6. Run `git status` after committing to verify success.

$ARGUMENTS
