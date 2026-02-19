# Cocoon Agent Development Style Guide

All agents must read this file during initialization, before analysis or code changes.

## Context Usage

### Rule

Context must flow through function parameters, not be created inside functions.

### Tests

- Use `t.Context()` as the default context in all test code.
- Use `context.Background()` only when a fresh root context is explicitly required
  (e.g., testing cancellation behavior where the test context must not interfere).
- When deriving a context (WithTimeout, WithCancel), prefer `t.Context()` as the parent.

```go
// Good
func TestFoo(t *testing.T) {
    err := doSomething(t.Context(), arg)
}

// Good - derived from t.Context()
func TestTimeout(t *testing.T) {
    ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
    defer cancel()
    err := doSomething(ctx, arg)
}

// Bad
func TestFoo(t *testing.T) {
    err := doSomething(context.Background(), arg)
}
```

### Production Code

- Never use `context.Background()` inside business logic. The only allowed
  call site is the application entry point (`main.go`) where the root context
  is created (e.g., `signal.NotifyContext(context.Background(), ...)`).
- Use `context.TODO()` as a placeholder when a function does not yet receive
  a context from its caller but should. This marks it as technical debt.
- Always pass context through function parameters rather than creating a new
  one inside a function, unless you are intentionally deriving a child context
  (WithTimeout, WithCancel, WithValue).

```go
// Good - context flows from caller
func (m *manager) Start(ctx context.Context, vmID string) error {
    // use ctx throughout
}

// Good - deriving a child context with timeout
func waitForBoot(ctx context.Context, ...) error {
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
}

// Bad - creating a new root context inside a function
func (m *manager) doWork() {
    ctx := context.Background() // should receive ctx as parameter
}
```

## Pre-Commit Checklist

### Rule

Always run `make lint` and `make test` before every commit and push on both platform (linux & darwin). No exceptions.

### Steps

```bash
make lint    # Must report 0 issues, include drawin and linux
make test    # All tests must pass
git add ...
git commit ...
git push
```

If either command fails, fix the issues before committing. Do not use
`--no-verify` or skip checks.

## Git Workflow

### Rule

When merging pull requests, use **rebase merge** by default to keep history linear.
Do not use merge-commit strategy unless explicitly required.

### Steps

```bash
# Preferred merge strategy
gh pr merge <PR_NUMBER> --rebase
```

## PR Review

### Rule

Every PR review must produce two outcomes via the `gh` CLI:

1. **Overall assessment** -- a general review comment summarizing the verdict
   (LGTM / request changes), scope check, CI status, and key observations.
2. **Inline code comments** -- specific findings attached to the exact file and
   line in the diff, just like clicking "+" on a line in the GitHub UI.

### Steps

```bash
# 1. Gather context
gh pr view <PR_NUMBER>
gh pr diff <PR_NUMBER>
gh pr checks <PR_NUMBER>

# 2. Post overall assessment as a review comment
cat > /tmp/pr-review.md <<'EOF'
LGTM. <one-line summary>

Verified:
- <bullet list of what was checked>
EOF
gh pr review <PR_NUMBER> --comment --body-file /tmp/pr-review.md

# 3. Post inline comments on specific code locations
#    Use the GitHub Reviews API to attach comments to exact diff lines.
#    Each comment targets a file path + line number in the PR head commit.
cat > /tmp/pr-inline.json <<'ENDJSON'
{
  "commit_id": "<HEAD_SHA>",
  "event": "COMMENT",
  "body": "Inline review comments.",
  "comments": [
    {
      "path": "path/to/file.go",
      "line": 42,
      "body": "nit: consider renaming for clarity."
    },
    {
      "path": "path/to/other.go",
      "line": 100,
      "body": "suggestion: add nosuid,nodev mount options for security hardening."
    }
  ]
}
ENDJSON
gh api repos/CMGS/cocoon/pulls/<PR_NUMBER>/reviews --input /tmp/pr-inline.json
```

### Comment Guidelines

- **Overall comment**: include verdict, CI status, scope boundaries, and
  non-line-specific observations.
- **Inline comments**: use for concrete code findings -- bugs, style issues,
  security concerns, suggestions. Prefix with severity:
  - `bug:` for correctness issues
  - `nit:` for style / cosmetic
  - `suggestion:` for non-blocking improvements
  - `question:` for clarification requests
- The `line` field refers to the line number in the **new version** of the file
  (the PR head), not the base. Use `gh pr diff` output to identify the correct
  line numbers.
- Get the HEAD SHA via: `gh api repos/CMGS/cocoon/pulls/<PR_NUMBER> --jq '.head.sha'`

### Replying to Inline Conversations

When responding to an existing inline conversation (e.g., answering a question,
acknowledging a fix, or continuing a discussion), **always reply within the same
conversation thread**. Do not post a new top-level PR comment or create a new
inline comment on the same line -- reply to the existing one.

Use the Pull Request Review Comments API with `in_reply_to`:

```bash
# 1. Find the comment ID to reply to
gh api repos/CMGS/cocoon/pulls/<PR_NUMBER>/comments \
  --jq '.[] | {id, path, line, body: .body[:80]}'

# 2. Reply within the same conversation thread
gh api repos/CMGS/cocoon/pulls/<PR_NUMBER>/comments \
  -f body="Acknowledged. Fixed in the next push." \
  -F in_reply_to=<COMMENT_ID>
```

**Do not**:
- Post a top-level `gh pr comment` to respond to an inline finding.
- Create a new inline comment on the same file/line instead of replying.
- Use `gh pr review --comment` for responses to specific conversations.

Each inline conversation should remain self-contained: the original finding and
all follow-up discussion stay in one thread, matching the GitHub UI behavior.

## Planning Records

### Rule

All plans must be recorded in `TODO.<date>.md` files under `dev/`.

### Naming

- Use date format `YYYY-MM-DD`.
- File pattern: `dev/TODO.YYYY-MM-DD.md`.
- For the same day, append updates to the existing file instead of creating
  multiple files.

### Steps

```bash
# Example for 2026-02-14
touch dev/TODO.2026-02-14.md

# Write/append plan items in this file before implementation
```
