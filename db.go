package main

import (
	"database/sql"
	"math/rand"
	"strings"
)

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS events (
  id          INTEGER PRIMARY KEY,
  artist      TEXT NOT NULL,
  title       TEXT NOT NULL,
  venue       TEXT NOT NULL,
  city        TEXT NOT NULL,
  starts_at   TEXT NOT NULL,
  description TEXT NOT NULL,
  gradient    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS seats (
  id           INTEGER PRIMARY KEY,
  event_id     INTEGER NOT NULL REFERENCES events(id),
  section      TEXT NOT NULL,
  row          TEXT NOT NULL,
  number       INTEGER NOT NULL,
  price_cents  INTEGER NOT NULL,
  status       TEXT NOT NULL DEFAULT 'available' CHECK (status IN ('available','held','sold')),
  hold_session TEXT,
  UNIQUE(event_id, section, row, number)
);
CREATE TABLE IF NOT EXISTS orders (
  id          INTEGER PRIMARY KEY,
  session_id  TEXT NOT NULL,
  name        TEXT NOT NULL,
  email       TEXT NOT NULL,
  total_cents INTEGER NOT NULL,
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE IF NOT EXISTS tickets (
  id       INTEGER PRIMARY KEY,
  order_id INTEGER NOT NULL REFERENCES orders(id),
  seat_id  INTEGER NOT NULL REFERENCES seats(id),
  code     TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS promos (
  id          INTEGER PRIMARY KEY,
  code        TEXT NOT NULL UNIQUE,
  percent_off INTEGER NOT NULL CHECK (percent_off BETWEEN 1 AND 100),
  expires_at  TEXT,
  max_uses    INTEGER,
  uses        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS waitlist (
  id               INTEGER PRIMARY KEY,
  event_id         INTEGER NOT NULL REFERENCES events(id),
  session_id       TEXT NOT NULL,
  email            TEXT NOT NULL,
  status           TEXT NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting','offered','claimed','expired','cancelled')),
  offered_seat_id  INTEGER REFERENCES seats(id),
  offered_at       TEXT,
  offer_expires_at TEXT,
  created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_seats_event ON seats(event_id);
CREATE INDEX IF NOT EXISTS idx_seats_hold ON seats(hold_session);
CREATE INDEX IF NOT EXISTS idx_tickets_order ON tickets(order_id);
CREATE TABLE IF NOT EXISTS transfers (
  id          INTEGER PRIMARY KEY,
  ticket_id   INTEGER NOT NULL REFERENCES tickets(id),
  old_code    TEXT NOT NULL,
  new_code    TEXT NOT NULL,
  holder_name  TEXT NOT NULL,
  holder_email TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS resales (
  id              INTEGER PRIMARY KEY,
  ticket_id       INTEGER NOT NULL REFERENCES tickets(id),
  event_id        INTEGER NOT NULL REFERENCES events(id),
  seller_order_id INTEGER NOT NULL REFERENCES orders(id),
  buyer_order_id  INTEGER NOT NULL REFERENCES orders(id),
  price_cents     INTEGER NOT NULL,
  fee_cents       INTEGER NOT NULL,
  payout_cents    INTEGER NOT NULL,
  created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_waitlist_event ON waitlist(event_id, status);
CREATE INDEX IF NOT EXISTS idx_transfers_old_code ON transfers(old_code);
CREATE INDEX IF NOT EXISTS idx_transfers_ticket ON transfers(ticket_id);
CREATE INDEX IF NOT EXISTS idx_resales_event ON resales(event_id, created_at);
`)
	if err != nil {
		return err
	}
	// columns added after initial ship — kept as guarded ALTERs for existing DBs
	if err := ensureColumn(db, "orders", "promo_code", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "orders", "discount_cents", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "orders", "status", "TEXT NOT NULL DEFAULT 'paid'"); err != nil {
		return err
	}
	if err := ensureColumn(db, "orders", "refunded_at", "TEXT"); err != nil {
		return err
	}
	// '' marks pre-tier rows that still need backfilling
	if err := ensureColumn(db, "seats", "tier", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "seats", "stage_rank", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "tickets", "price_cents", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "orders", "group_discount_cents", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "tickets", "checked_in_at", "TEXT"); err != nil {
		return err
	}
	// audit only — records which door credential scanned the ticket; never
	// rendered back to clients
	if err := ensureColumn(db, "tickets", "checked_in_by", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// '' marks events that still need a door code generated
	if err := ensureColumn(db, "events", "checkin_code", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// '' means the holder is still the order's buyer
	if err := ensureColumn(db, "tickets", "holder_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "tickets", "holder_email", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// 0 = not listed for resale
	if err := ensureColumn(db, "tickets", "listing_price_cents", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// 1 = this code died when the seat was resold; resale_price_cents is what the buyer paid
	if err := ensureColumn(db, "tickets", "resold", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(db, "tickets", "resale_price_cents", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// 'resale' marks tickets bought on resale — they never count in venue sales
	if err := ensureColumn(db, "tickets", "kind", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// snapshot what was actually paid onto each ticket — reports read this so
	// re-pricing seats never rewrites sales history
	if _, err := db.Exec(`UPDATE tickets SET price_cents = (SELECT price_cents FROM seats WHERE seats.id = tickets.seat_id) WHERE price_cents = 0`); err != nil {
		return err
	}
	if err := backfillCheckinCodes(db); err != nil {
		return err
	}
	// partial index: '' means "no code yet" and must not collide across rows.
	// Runs after the backfill so existing DBs never present duplicate ''s.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_events_checkin ON events(checkin_code) WHERE checkin_code <> ''`); err != nil {
		return err
	}
	if err := backfillTiers(db); err != nil {
		return err
	}
	return backfillStageRank(db)
}

// backfillCheckinCodes gives every event a door code — the shareable
// credential that opens the /checkin scanner page for that event. Idempotent.
func backfillCheckinCodes(db *sql.DB) error {
	rows, err := db.Query(`SELECT id FROM events WHERE checkin_code = '' OR checkin_code IS NULL`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := db.Exec(`UPDATE events SET checkin_code=? WHERE id=?`, "TPK-"+strings.ToUpper(newID(8)), id); err != nil {
			return err
		}
	}
	return nil
}

// backfillStageRank orders each event's sections nearest-stage-first using
// their top seat price as the proximity signal. Idempotent.
func backfillStageRank(db *sql.DB) error {
	evRows, err := db.Query(`SELECT id FROM events`)
	if err != nil {
		return err
	}
	var eventIDs []int64
	for evRows.Next() {
		var id int64
		if err := evRows.Scan(&id); err != nil {
			evRows.Close()
			return err
		}
		eventIDs = append(eventIDs, id)
	}
	evRows.Close()
	for _, id := range eventIDs {
		secs, err := db.Query(`SELECT section FROM seats WHERE event_id=? GROUP BY section ORDER BY MAX(price_cents) DESC`, id)
		if err != nil {
			return err
		}
		var names []string
		for secs.Next() {
			var n string
			if err := secs.Scan(&n); err != nil {
				secs.Close()
				return err
			}
			names = append(names, n)
		}
		secs.Close()
		for rank, name := range names {
			if _, err := db.Exec(`UPDATE seats SET stage_rank=? WHERE event_id=? AND section=?`, rank, id, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// seatTiers maps a row's position within its section to a pricing tier:
// front third Premium, middle Standard, back third Value.
type seatTier struct {
	name string
	mult float64
}

func tierForRow(rowIdx, totalRows int) seatTier {
	premiumRows, valueRows := totalRows/3, totalRows/3
	if premiumRows < 1 {
		premiumRows = 1
	}
	if valueRows < 1 {
		valueRows = 1
	}
	switch {
	case rowIdx < premiumRows:
		return seatTier{"Premium", 1.5}
	case rowIdx >= totalRows-valueRows:
		return seatTier{"Value", 0.7}
	default:
		return seatTier{"Standard", 1.0}
	}
}

// backfillTiers assigns tiers to pre-tier seats and re-prices ones still on
// sale. Sold and held seats keep their price; history lives on tickets.
func backfillTiers(db *sql.DB) error {
	secRows, err := db.Query(`SELECT DISTINCT event_id, section FROM seats WHERE tier = '' OR tier IS NULL`)
	if err != nil {
		return err
	}
	type sec struct {
		eventID int64
		name    string
	}
	var secs []sec
	for secRows.Next() {
		var s sec
		if err := secRows.Scan(&s.eventID, &s.name); err != nil {
			secRows.Close()
			return err
		}
		secs = append(secs, s)
	}
	secRows.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range secs {
		rowLabels, err := tx.Query(`SELECT DISTINCT row FROM seats WHERE event_id=? AND section=? ORDER BY row`, s.eventID, s.name)
		if err != nil {
			return err
		}
		var labels []string
		for rowLabels.Next() {
			var l string
			if err := rowLabels.Scan(&l); err != nil {
				rowLabels.Close()
				return err
			}
			labels = append(labels, l)
		}
		rowLabels.Close()
		for i, l := range labels {
			t := tierForRow(i, len(labels))
			if _, err := tx.Exec(`UPDATE seats SET tier = ? WHERE event_id=? AND section=? AND row=?`, t.name, s.eventID, s.name, l); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE seats SET price_cents = CAST(price_cents * ? AS INTEGER) / 100 * 100
				WHERE event_id=? AND section=? AND row=? AND status='available'`, t.mult, s.eventID, s.name, l); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func ensureColumn(db *sql.DB, table, column, def string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + def)
	return err
}

type sectionSpec struct {
	name        string
	basePrice   int64
	rows        int
	seatsPerRow int
}

func seed(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM promos`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`INSERT INTO promos (code,percent_off,expires_at,max_uses) VALUES ('ENCORE20',20,'2026-12-31T23:59:59Z',500)`); err != nil {
			return err
		}
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	type ev struct {
		artist, title, venue, city, startsAt, desc, gradient string
		priceMult                                            float64
	}
	events := []ev{
		{"Nova Reign", "Midnight Static Tour", "The Wiltern", "Los Angeles, CA", "2026-11-14T19:00:00-08:00",
			"An explosive live set spanning three albums of synth-driven alt rock, plus unreleased material.",
			"from-fuchsia-600 via-purple-700 to-indigo-900", 1.0},
		{"Glass Pony", "Chromatic World Tour", "The Fillmore", "San Francisco, CA", "2026-11-21T20:00:00-08:00",
			"The indie-pop quartet brings its chart-topping third record to the stage with a full light show.",
			"from-cyan-500 via-teal-600 to-emerald-800", 0.9},
		{"Mara Voss", "Velvet Hour", "Radio City Music Hall", "New York, NY", "2026-12-03T19:30:00-05:00",
			"One night only. A stripped-back orchestral arrangement of the platinum record Velvet Hour.",
			"from-rose-500 via-pink-600 to-rose-900", 1.4},
		{"Dead Signal", "Feedback Loop Tour", "Red Rocks Amphitheatre", "Morrison, CO", "2026-11-28T18:30:00-07:00",
			"Post-punk legends return to Red Rocks for their loudest show yet. Rain or shine.",
			"from-amber-500 via-orange-600 to-red-800", 1.1},
		{"Juno & The Wilds", "Harvest Moon Sessions", "The Ryman Auditorium", "Nashville, TN", "2026-12-11T20:00:00-06:00",
			"Folk anthems and foot-stomping Americana in the mother church of country music.",
			"from-lime-500 via-green-600 to-teal-800", 0.8},
		{"PYRAMIDS", "Neon Desert Tour", "The Sphere", "Las Vegas, NV", "2026-12-19T21:00:00-08:00",
			"A full-sensory electronic experience with 360-degree visuals. Standing room pit available.",
			"from-violet-600 via-fuchsia-600 to-purple-900", 1.5},
	}

	sections := []sectionSpec{
		{"Pit", 18500, 4, 16},
		{"Floor", 12500, 8, 20},
		{"Lower Bowl", 8900, 6, 24},
		{"Upper Bowl", 4900, 6, 24},
	}
	rowLetters := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	evStmt, err := tx.Prepare(`INSERT INTO events (artist,title,venue,city,starts_at,description,gradient,checkin_code) VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	seatStmt, err := tx.Prepare(`INSERT INTO seats (event_id,section,row,number,price_cents,status,tier,stage_rank) VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}

	for i, e := range events {
		res, err := evStmt.Exec(e.artist, e.title, e.venue, e.city, e.startsAt, e.desc, e.gradient, "TPK-"+strings.ToUpper(newID(8)))
		if err != nil {
			return err
		}
		eventID, _ := res.LastInsertId()
		rng := rand.New(rand.NewSource(int64(1000 + i)))
		for si, s := range sections {
			price := int64(float64(s.basePrice)*e.priceMult) / 100 * 100 // round to whole dollars
			for r := 0; r < s.rows; r++ {
				t := tierForRow(r, s.rows)
				rowPrice := int64(float64(price)*t.mult) / 100 * 100
				for seatNum := 1; seatNum <= s.seatsPerRow; seatNum++ {
					status := "available"
					if rng.Float64() < 0.15 {
						status = "sold"
					}
					if _, err := seatStmt.Exec(eventID, s.name, string(rowLetters[r]), seatNum, rowPrice, status, t.name, si); err != nil {
						return err
					}
				}
			}
		}
	}
	return tx.Commit()
}
