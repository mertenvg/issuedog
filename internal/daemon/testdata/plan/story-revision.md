### Plan for #124 (rev 2)

A story plan has tasks: the booking change grows, and the production setting is no longer needed.

**Today** each quote records its expiry time (T1, done), and the setting for its lifetime waits in production (T3).

This revision:
- keeps T1, which is done;
- changes T2, so an expired quote's error also says when it expired;
- drops T3, since the lifetime now has a default in the service;
- adds T4, the default lifetime of 24 hours.

**Changes since rev 1.**

The discussion settled on a default lifetime, so production needs no setting, and the expired error names the time it expired.

**Assumptions to confirm with your 👍:**
- A default of 24 hours is right for every environment that sets none.

**T1. Record when each quote expires** · kept

**T2. Refuse an expired quote at booking, saying when it expired** · changed · scope:booking · p1
Outcome: Booking an expired quote returns an expired error that says when it expired, with a link to a new quote, and books nothing.
- Booking a quote before its expiry time works as it does today.
- Booking a quote at or after its expiry time returns 410 with the code quote_expired and the expiry time, and books nothing.
- The error's body links to a new quote for the same trip.
- The MCP server's book tool returns the same error.

Depends on: T1, T4

**T3. Set the quote lifetime in production** · dropped

**T4. Give the quote lifetime a default** · new · scope:booking · p2
Outcome: The booking service runs with a quote lifetime of 24 hours unless its settings give another.
- With no lifetime in its settings, the booking service starts, and quotes expire after 24 hours.
- A lifetime in its settings that isn't a positive duration still stops the service from starting.

Depends on: T1

**Order:** T4, then T2

Validation (11.3): 3 tasks, within `plan.max_tasks` of 10, each with one scope from the list; every child listed once, kept, changed or dropped, and each closed one kept; no dependency cycles; 4 sub-issues in all, within 100; at most 2 dependency links of one type per issue, within 50.

cc @mertenvg

**To approve this plan, react 👍 to this comment.** Any other comment sends it back for revision.
