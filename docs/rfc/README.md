# Request for Comments (RFC) Process

## Overview

The RFC process is used for proposing significant changes to Cocoon's architecture, design, or public interfaces. Not all changes require an RFC - use this process for changes that:

- Introduce new major features or subsystems
- Change existing public APIs or behavior in backwards-incompatible ways
- Require community input or consensus
- Have significant trade-offs that warrant discussion

## When to Write an RFC

**Use an RFC for**:
- Major architectural changes (e.g., switching from REST to gRPC)
- New subsystems (e.g., adding network policy engine)
- Breaking changes to CLI or API
- Significant performance or security trade-offs

**Don't use an RFC for**:
- Bug fixes
- Internal refactoring that doesn't change public behavior
- Documentation improvements
- Minor feature additions

## RFC Template

See [TEMPLATE.md](./TEMPLATE.md) for the RFC template.

## RFC Process

### 1. Create RFC Document

```bash
# Copy template
cp docs/rfc/TEMPLATE.md docs/rfc/NNN-your-feature-name.md

# Fill in the template
# NNN = next available RFC number (e.g., 001, 002, 003)
```

### 2. Open Pull Request

- Title: `RFC NNN: Your Feature Name`
- Description: Brief summary and motivation
- Label: `rfc`
- Add reviewers from relevant teams

### 3. Discussion Period

- Minimum 1 week for discussion
- Address feedback and update RFC
- RFCs are living documents during discussion

### 4. Resolution

**Accepted**: RFC merged, implementation can begin
**Rejected**: RFC closed with rationale documented
**Withdrawn**: Author closes RFC

## RFC Numbering

RFCs are numbered sequentially starting from 001 (e.g., `001-feature-name.md`).

> **Note:** The initial Cocoon CLI architecture was captured directly in the main documentation (docs 00-10) rather than as a formal RFC. The first RFC number (001) is available for future proposals.

## RFC Status

Each RFC has a status field:

- **Draft**: Under active discussion
- **Accepted**: Approved for implementation
- **Implemented**: Feature shipped
- **Rejected**: Not accepted
- **Withdrawn**: Author withdrew proposal
- **Superseded**: Replaced by another RFC

## Active RFCs

Currently, there are no active RFCs. The initial Cocoon design was documented in the main docs (00-overview.md through 10-implementation-roadmap.md).

Future RFCs will be listed here as they are created.

## Historical RFCs

No formal RFCs have been created. The initial architecture was captured directly in the main documentation (00-overview.md through 10-implementation-roadmap.md) rather than through the RFC process.

## Questions?

For questions about the RFC process, open an issue with the `question` label.
