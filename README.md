# ticketpunch

A demo concert ticketing app written in Go with SQLite. Browse events, pick seats
on an interactive map, check out with a promo code, and get a ticket with a code
like `TP-4F9A1C22B7`. Hosts get an admin sales report and a door check-in page.

## Run it

```sh
go run .
```

Then open http://localhost:8080. The app listens on port 8080 and stores data in
`./tickets.db` (created and seeded on first run — delete it to start over).

## Tests

```sh
go test ./...
```

## Transfers and resale

A ticket's code is the credential — whoever holds the current code is the holder.

| Route | What it does |
| --- | --- |
| `GET /tickets/{code}` | Ticket page (QR, seat, holder, holder history). Answers JSON for `Accept: application/json`. |
| `POST /tickets/{code}/transfer` | Hands the ticket to a new holder (`name`, `email`). Mints a new code; the old code dies everywhere (410). |
| `POST /tickets/{code}/list` | Puts the ticket up for fan resale at `price` dollars. |
| `POST /tickets/{code}/delist` | Takes the listing down. |

Rules:

- A ticket can be transferred at most **3 times**. Checked-in, refunded,
  resold, and currently listed tickets cannot be transferred.
- Resale price is capped at **110% of the original face value**.
- When a resale sells, the venue takes a **5% fee** and the seller's code dies
  (410, `resold`). The buyer gets a fresh code with a fresh holder history.
- On a **sold-out event a listing goes to the waitlist first** — the first
  eligible waiter gets the offer, and only when nobody is left does the seat
  appear on the open map under "Fan resale".
- `OFFER_TTL_SECONDS` overrides the 15-minute waitlist offer window at startup
  (must be a positive whole number of seconds; default 900).
- Promos and group deals never apply to resale seats.
- An order cannot be refunded if any ticket in it was transferred or resold,
  is currently listed, or was itself bought on resale.
