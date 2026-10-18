### Plan for #123 (rev 1)

Expiry, showing it and reporting on it are three outcomes customers and support see separately, so this plan has stories.

**Today** a quote never expires: booking accepts a quote of any age, at its old price.

This plan adds:
- expiry at booking, in the booking API, so the PWA and the MCP server get it together (S1);
- the expiry time on every quote a customer sees (S2);
- expired quotes in the monthly report, marked as expired (S3).

**Assumptions to confirm with your 👍:**
- A quote's lifetime is 24 hours for every customer, as the spec says.
- Expiry is checked only when a quote is booked, so nothing runs in the background.
- An expired quote is kept, so support can still look it up.

**S1. Quotes expire after 24 hours** · p1 · model:opus · effort:high
Outcome: Booking a quote more than 24 hours old is refused, with a link to a new quote.
- Booking a quote less than 24 hours old works as it does today.
- Booking a quote more than 24 hours old returns an expired error and books nothing.
- The expired error links to a new quote for the same trip.
- The PWA and the MCP server both get the expired error.

Depends on: nothing

**S2. Customers see when a quote expires** · p1
Outcome: Every quote a customer sees shows the time it expires.
- A new quote in the PWA shows its expiry time in the customer's time zone.
- A quote from the MCP server includes its expiry time.
- A quote shown after it expired says it has expired and offers a new quote.

Depends on: S1

**S3. Expired quotes in the monthly report** · p2 · effort:medium
Outcome: The monthly report lists expired quotes, marked as expired.
- An expired quote appears in the report for the month it was issued.
- The report marks each expired quote as expired.
- The report's totals count booked quotes only.

Depends on: S1

**Order:** S1, then S2 and S3 in parallel

Validation (11.3): 3 stories, with no tasks; no dependency cycles; 3 sub-issues in all, within 100; at most 2 dependency links of one type per issue, within 50.

cc @mertenvg

**To approve this plan, react 👍 to this comment.** Any other comment sends it back for revision.
