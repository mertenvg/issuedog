### Plan for #124 (rev 1)

A story plan has tasks: two PRs in booking, and a production setting only the owner can change.

**Today** a quote records when it was issued, but nothing reads that at booking.

This plan adds:
- each quote's expiry time, from a lifetime the booking service reads from its settings (T1);
- the expired error at booking, with a link to a new quote (T2);
- the lifetime set in production, which only the owner can change (T3).

Your answers are used: the lifetime is a setting, 24 hours in production, and the MCP server's changes stay in `booking`, since it calls the same booking API.

**Assumptions:**
- The lifetime has no default, so the service refuses to start without it and production can't run without expiry by mistake.
- An expired quote is refused only at booking, so quotes already booked are unaffected.

**T1. Record when each quote expires** · scope:booking · p2 · model:sonnet · effort:medium
Outcome: Every quote knows its expiry time, from the lifetime in the booking service's settings.
- A quote's expiry time is its issue time plus the configured lifetime.
- The booking service refuses to start when the lifetime is missing or isn't a positive duration.
- The quote API returns each quote's expiry time.

Depends on: nothing

**T2. Refuse an expired quote at booking** · scope:booking · p1
Outcome: Booking an expired quote returns an expired error with a link to a new quote, and books nothing.
- Booking a quote before its expiry time works as it does today.
- Booking a quote at or after its expiry time returns 410 with the code quote_expired, and books nothing.
- The error's body links to a new quote for the same trip.
- The MCP server's book tool returns the same error.

Depends on: T1, T3

**T3. Set the quote lifetime in production** · scope:infra · p2 · human
Outcome: Production runs with a quote lifetime of 24 hours.
- Production's settings on main set the quote lifetime to 24 hours.

Done when: Production's settings on `main` set `QUOTE_TTL=24h`. (checked automatically: file)
Depends on: nothing

**Order:** T1 and T3 in parallel, then T2

Validation (11.3): 3 tasks, within `plan.max_tasks` of 10, each with one scope from the list; no dependency cycles; 3 sub-issues in all, within 100; at most 2 dependency links of one type per issue, within 50.

This plan skips review, as `plan.review` is false: #124 moves straight to `planned`.
