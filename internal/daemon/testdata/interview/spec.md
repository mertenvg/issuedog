## Problem

Quotes never expire, so customers book at prices that are out of date.

Support then has to honour the old price or cancel the booking.

## Goals

- A quote expires 24 hours after it's issued.
- Expired quotes stay available for reporting.

## Out of scope

- Changing how prices are calculated.

## Users and flows

A customer asks for a quote, then books it:

1. The quote shows when it expires.
2. Booking an expired quote is refused, with a link to a new quote.

## Constraints

- The PWA and the MCP server use the same booking API.

## Acceptance criteria

- Booking a quote more than 24 hours old returns an expired error.
- An expired quote still appears in the monthly report.
- An unticked box is stripped.
- So is a ticked one.

## Decisions

- Q1.1 → a: Expiry is enforced server-side on booking. (mertenvg, round 1)
- Q1.2 → superseded by Q2.1
- Q2.1 → free text: Expired quotes are kept and marked. (merten_acme, round 2)
- Added, not asked: The expiry time is shown on the quote.
