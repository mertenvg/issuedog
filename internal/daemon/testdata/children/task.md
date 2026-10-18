**Parent:** #124   **Scope:** scope:booking   **Priority:** p1

## Context

- `internal/booking/book.go`: `Book` checks the quote's status and creates the booking in one transaction.
- `internal/api/errors.go`: `writeError` maps a booking error to a status and a code.
- `internal/mcp/tools.go`: the `book` tool calls `Book` and returns its error's code.

## Change

- `internal/booking/book.go`: in the transaction, return `ErrQuoteExpired` when now is at or after `ExpiresAt`, before anything is written.
- `internal/api/errors.go`: map `ErrQuoteExpired` to 410 and `quote_expired`, with a `new_quote` link built from the quote's trip.
- `internal/mcp/tools.go`: no change beyond a test, since it passes the code through.

## Acceptance criteria

- Booking a quote before its expiry time works as it does today.
- Booking a quote at or after its expiry time returns 410 with the code quote_expired, and books nothing.
- The error's body links to a new quote for the same trip.
- The MCP server's book tool returns the same error.

## Definition of done

- Tests added or updated for every changed behavior
- verify.commands pass
- No new skip markers

## Test plan

- `internal/booking/book_test.go`: booking one second before the expiry time works; at it, and after it, gives `ErrQuoteExpired` and writes no booking.
- `internal/api/book_test.go`: the 410 body has `quote_expired` and the `new_quote` link.
- `internal/mcp/tools_test.go`: the `book` tool returns `quote_expired`.

<!-- issuedog:origin plan=#124 rev=1 item=T2 -->
