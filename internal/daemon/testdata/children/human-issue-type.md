**Parent:** #124   **Scope:** scope:infra   **Priority:** p2

**Goal:** Set the quote lifetime to 24 hours in production's settings.
**Why it blocks:** #126 can't proceed until production has a quote lifetime, because the booking service won't start without one, and the deploy settings are a path only the owner can change.
**Steps:**
1. On a new branch, open `deploy/production.env`.
2. Add the line `QUOTE_TTL=24h`.
3. Open a PR, check its diff is only that line, and merge it into `main`.

**Links:**
- https://github.com/acme/booking/blob/main/deploy/production.env

**Done when:** This repo's owner, an organization, has the issue type `Human task`, enabled. I check this when you reply `/done` or react 👍 to this issue.
**Estimated time:** 5 min

cc @mertenvg

<!-- issuedog:origin plan=#124 rev=1 item=T3 check=issue-type human=1 value=Human%20task -->
