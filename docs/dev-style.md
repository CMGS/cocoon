# Cocoon Development Style Guide

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

// Good — derived from t.Context()
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
// Good — context flows from caller
func (m *manager) Start(ctx context.Context, vmID string) error {
    // use ctx throughout
}

// Good — deriving a child context with timeout
func waitForBoot(ctx context.Context, ...) error {
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
}

// Bad — creating a new root context inside a function
func (m *manager) doWork() {
    ctx := context.Background() // should receive ctx as parameter
}
```

## Pre-Commit Checklist

### Rule

Always run `make lint` and `make test` before every commit and push. No exceptions.

### Steps

```bash
make lint    # Must report 0 issues
make test    # All tests must pass
git add ...
git commit ...
git push
```

If either command fails, fix the issues before committing. Do not use
`--no-verify` or skip checks.
