# Security Policy

## Supported versions

`sqi` is currently in pre-release development. Until a stable v1.0 is tagged, only the latest commit on the `main` branch receives security fixes. There is no backport policy for pre-release versions.

Once stable releases begin, this table will be updated to reflect which release lines are actively supported.

| Version | Supported |
|---|---|
| `main` (pre-release) | ✅ Yes |
| All others | ❌ No |

## Reporting a vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.** Public disclosure before a fix is available puts all users at risk.

### Preferred: GitHub private vulnerability reporting

Use GitHub's built-in private reporting to submit a vulnerability report confidentially:

1. Go to the [sqi Security Advisories page](../../security/advisories/new)
2. Click **Report a vulnerability**
3. Fill in the details — affected component, reproduction steps, potential impact, and any suggested mitigations you have in mind

This creates a private thread visible only to you and the maintainers and is the fastest path to a coordinated fix.

### Alternative: email

If you are unable to use GitHub's private reporting, email the maintainers directly:

**security@uberware.net**

Include as much of the following as you can:

- A clear description of the vulnerability
- The component or file(s) affected
- Steps to reproduce or a proof-of-concept
- The potential impact (e.g. remote code execution, privilege escalation, information disclosure)
- Any suggested mitigations

## What to expect

| Milestone | Target timeframe |
|---|---|
| Acknowledgement of your report | Within 2 business days |
| Initial assessment and severity triage | Within 5 business days |
| Fix or workaround available | Depends on severity and complexity |
| Public disclosure | Coordinated with you after the fix ships |

We will keep you informed at each stage. If you do not receive an acknowledgement within 2 business days, follow up by email to ensure your report was received.

## Disclosure policy

We follow coordinated disclosure. This means:

- We ask that you give us a reasonable amount of time to investigate and fix the issue before disclosing it publicly.
- We will credit you in the security advisory unless you prefer to remain anonymous.
- We aim to publish a security advisory on GitHub and release a patched version simultaneously.
- For critical vulnerabilities we target a 90-day fix window from the date of acknowledgement, in line with industry norms. We will communicate openly if we expect to need more time.

## Scope

The following are in scope for security reports:

- `sqi-server` — scheduler, REST API, WebSocket, embedded NATS, SQLite state management
- `sqi-worker` — task executor and worker agent
- The Python client library (`sqi-sdk`)
- The authentication and authorization implementation (once shipped — Phase 3+)
- Dependency vulnerabilities in the Go module graph or npm packages

The following are **out of scope**:

- Vulnerabilities in third-party software that `sqi` integrates with but does not control (e.g. Arnold, Houdini, Nuke)
- Issues in the community preset library that are specific to a user's own environment configuration
- Theoretical vulnerabilities with no practical exploit path
- Social engineering of maintainers

## Safe harbor

We consider good-faith security research to be a valuable contribution. We will not pursue legal action against researchers who report vulnerabilities responsibly in accordance with this policy, provided that the research does not access, modify, or exfiltrate data belonging to other users, and does not disrupt production services.
