**Parent:** none   **Scope:** scope:booking   **Priority:** p2

## Context

Follow-up to #98.

Booking checks a quote's expiry in `booking/quote.go`, but `isExpired` compares the quote's issue time in the customer's time zone, so a quote can be booked for hours after it expired.

## Change

Compare in UTC in `isExpired` in `booking/quote.go`, and add the cases to `booking/quote_test.go`.

## Acceptance criteria

- Booking a quote issued 24 hours and 1 minute ago, in any time zone, returns an expired error.
- The original behavior of #98 is unaffected.

## Definition of done

- Tests added or updated for every changed behavior
- verify.commands pass
- No new skip markers

## Test plan

`TestIsExpired` in `booking/quote_test.go` covers a quote just inside and just past 24 hours, in UTC and in two other time zones.
