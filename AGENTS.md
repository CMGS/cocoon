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
  is created (e.g., `signal.NotifyContext(context.TODO(), ...)`).
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

## Error Scope Minimization

### Rule

Prefer `if err := fn(); err != nil` over `err := fn()` followed by
`if err != nil` to minimize the scope of the `err` variable.

This applies **only** when the error is the sole return value, or when
additional return values are not used after the if block. When you need
the non-error return value later, the two-line pattern is correct.

```go
// Good — err scoped to the if block
if err := doSomething(ctx, arg); err != nil {
    return fmt.Errorf("do something: %w", err)
}

// Good — err needed after the if (e.g., for multiple return values)
result, err := doSomething(ctx, arg)
if err != nil {
    return fmt.Errorf("do something: %w", err)
}
// result is used below...

// Bad — err scope unnecessarily wide
err := doSomething(ctx, arg)
if err != nil {
    return fmt.Errorf("do something: %w", err)
}
// err is never used again but remains in scope
```

## Pre-Commit Checklist

### Rule

Always run `make lint` and `make test` before every commit and push on both platform (linux & darwin). No exceptions.

### Steps

```bash
make lint    # Must report 0 issues, include darwin and linux
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

## PR Lifecycle Management

### Rule

All PR work must follow a standard lifecycle with explicit gates. A PR is not
"done" at merge time; post-merge issue synchronization is mandatory.

### Standard Lifecycle

1. **Draft**
2. **Ready**
3. **In Review**
4. **Changes Requested / Approved**
5. **CI Green**
6. **Rebase Merge**
7. **Post-Merge Sync**

### PR Ready Gate (before requesting review)

- [ ] PR links one implementation issue (`Closes #N` or explicit `Refs #N`)
- [ ] PR scope maps to concrete checklist items in the issue description
- [ ] Local quality gates passed: `make lint` and `make test`
- [ ] PR description includes summary, scope, validation steps, and risk notes

### Merge Ready Gate (before `gh pr merge --rebase`)

- [ ] CI required checks are green
- [ ] No unresolved review conversations remain
- [ ] Review findings are either fixed, explicitly accepted, or deferred with a tracking issue
- [ ] Branch rebased onto latest `main` when needed

### Post-Merge Synchronization (required)

After merge, do all of the following in the same execution window:

1. Update the issue **description checklist** (authoritative source of truth)
2. Update the issue's canonical execution/progress comment (if used)
3. If residual scope remains, open a follow-up issue and link it
4. Close the implementation issue only when checklist is fully settled
5. Verify local branch state is clean and on `main`

### Steps (gh CLI)

```bash
# 1) Merge with rebase
gh pr merge <PR_NUMBER> --rebase

# 2) Sync issue checklist in description
gh issue edit <ISSUE_NUMBER> --body-file /tmp/issue-body-updated.md

# 3) (Optional but recommended) update canonical progress comment
gh api repos/CMGS/cocoon/issues/comments/<COMMENT_ID> \
  -X PATCH --raw-field body="$(cat /tmp/progress-comment.md)"

# 4) Close or split scope
gh issue close <ISSUE_NUMBER> --comment "Completed in PR #<PR_NUMBER>"
# or create follow-up
gh issue create --title "<follow-up>" --body-file /tmp/follow-up.md
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
  - `security:` for vulnerabilities, unsafe defaults, or exploit paths
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

### Resolving Inline Conversations

An inline conversation is **resolved** only when both parties reach consensus:

- **Fixed**: The author pushed a fix and the reviewer confirmed it. Conversation
  can be resolved.
- **Accepted as-is**: The reviewer explicitly agrees the current code is correct
  after the author's explanation. Conversation can be resolved.
- **Deferred**: Both parties agree to track the item in a separate issue/PR.
  Conversation can be resolved with a link to the tracking issue.

**Do not** resolve a conversation if:
- The reviewer has not yet replied to the author's response.
- The reviewer disagrees with the author's rationale and discussion is ongoing.
- The item is dismissed without explanation (e.g., silently resolving without
  a reply).

In practice: leave conversations open until a confirming reply is posted
(e.g., "Confirmed", "Accepted", "Tracked in #N"). The person who raised the
finding is the one who should resolve it, not the author.

When a conversation reaches consensus (fixed/accepted/deferred), you must
explicitly mark that inline conversation as **resolved** in GitHub (do not
leave agreed threads open).

Use GraphQL to resolve the thread by ID:

```bash
# 1. List review thread IDs and resolution state
gh api graphql -f query='
  query($owner:String!, $repo:String!, $pr:Int!) {
    repository(owner:$owner, name:$repo) {
      pullRequest(number:$pr) {
        reviewThreads(first:100) {
          nodes { id isResolved }
        }
      }
    }
  }' -F owner=CMGS -F repo=cocoon -F pr=<PR_NUMBER>

# 2. Resolve a thread that reached consensus
gh api graphql -f query='
  mutation($threadId:ID!) {
    resolveReviewThread(input:{threadId:$threadId}) {
      thread { id isResolved }
    }
  }' -F threadId=<THREAD_ID>
```

## Issue Management

### Rule

Every issue that tracks implementation work must contain an **actionable
checklist in the issue description** (not in comments). The checklist is the
single source of truth for what remains to be done.

### Lifecycle States (Standard)

- `Open`: defined and not started
- `In Progress`: implementation active
- `Blocked`: cannot proceed due to external dependency/decision
- `Done`: all checklist items settled, awaiting close
- `Closed`: completed or explicitly dropped with rationale

### Workflow

1. **Create**: Write the issue body with a `## Checklist` section listing all
   known work items as `- [ ] ...` tasks.
2. **Discuss**: Use comments for design discussion, questions, and proposals.
   Comments may contain temporary task lists for brainstorming, but these are
   NOT the source of truth.
3. **Update description**: Once a decision is reached in comments (regardless
   of how many comments it took), update the **issue description checklist** to
   reflect the consensus. Do not leave agreed-upon work items only in comments.
4. **Check off**: Mark items `- [x]` as they are completed (via PR or direct
   commit). Reference the commit/PR in the checkbox line.
5. **Close**: Close the issue when all checklist items are checked or explicitly
   dropped with a rationale.
6. **Post-merge sync**: Immediately after related PR merge, reflect merged state
   in both issue description and canonical progress comment (if any).

### Format

```markdown
## Summary
<one paragraph describing the goal>

## Scope
- <in-scope item 1>
- <in-scope item 2>

## Out of Scope
- <explicitly excluded item 1>

## Acceptance Criteria
- [ ] <observable behavior 1>
- [ ] <observable behavior 2>

## Checklist
- [ ] Item 1: description (file/package scope)
- [ ] Item 2: description (file/package scope)
- [ ] Item 3: description (file/package scope)
```

### Steps (gh CLI)

```bash
# Create issue with checklist in body
gh issue create --title "fix: address codebase review findings" \
  --body-file /tmp/issue-body.md

# Update issue description after consensus
gh issue edit <NUMBER> --body-file /tmp/issue-body-updated.md

# Check off item (edit body, toggle checkbox)
gh issue edit <NUMBER> --body-file /tmp/issue-body-checked.md

# Close issue after merge + sync
gh issue close <NUMBER> --comment "Completed in PR #<PR_NUMBER>"
```

### Scope Split Rule

If any accepted requirement is not delivered in the current PR series:

- Open a follow-up issue with its own checklist
- Link it from the original issue body and closing comment
- Mark the original checklist item as split/deferred with the follow-up issue ID

### Do not

- Put the authoritative checklist in a comment. Comments are for discussion.
- Create multiple competing checklists across different comments.
- Leave agreed-upon items only in comments without updating the description.
- Close an issue while leaving unchecked items without rationale.

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
