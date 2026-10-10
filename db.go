package main

import (
	"database/sql"
	"math/rand"
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
CREATE INDEX IF NOT EXISTS idx_waitlist_event ON waitlist(event_id, status);
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
	return ensureColumn(db, "orders", "refunded_at", "TEXT")
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

	evStmt, err := tx.Prepare(`INSERT INTO events (artist,title,venue,city,starts_at,description,gradient) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	seatStmt, err := tx.Prepare(`INSERT INTO seats (event_id,section,row,number,price_cents,status) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}

	for i, e := range events {
		res, err := evStmt.Exec(e.artist, e.title, e.venue, e.city, e.startsAt, e.desc, e.gradient)
		if err != nil {
			return err
		}
		eventID, _ := res.LastInsertId()
		rng := rand.New(rand.NewSource(int64(1000 + i)))
		for _, s := range sections {
			price := int64(float64(s.basePrice)*e.priceMult) / 100 * 100 // round to whole dollars
			for r := 0; r < s.rows; r++ {
				for seatNum := 1; seatNum <= s.seatsPerRow; seatNum++ {
					status := "available"
					if rng.Float64() < 0.15 {
						status = "sold"
					}
					if _, err := seatStmt.Exec(eventID, s.name, string(rowLetters[r]), seatNum, price, status); err != nil {
						return err
					}
				}
			}
		}
	}
	return tx.Commit()
}
