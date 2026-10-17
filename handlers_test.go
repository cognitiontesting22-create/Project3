package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- helpers ----------

func setup(t *testing.T) {
	t.Helper()
	var err error
	// temp-file DB: same locking semantics as production (shared-cache
	// in-memory DBs deadlock on read-during-write-transaction)
	db, err = sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := seed(db); err != nil {
		t.Fatal(err)
	}
	tpl = parseTemplates()
	t.Cleanup(func() { db.Close() })
}

// do routes a request through a real mux so handlers relying on
// PathValue (e.g. /events/{id}) work correctly.
func do(t *testing.T, pattern string, h func(w http.ResponseWriter, r *http.Request), method, target, sid string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	if sid != "" {
		r = r.WithContext(withSession(r.Context(), sid))
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc(pattern, h)
	mux.ServeHTTP(rec, r)
	return rec
}

func queryInt(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func queryStr(t *testing.T, q string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

func holdSeat(t *testing.T, seatID int64, sid string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE seats SET status='held', hold_session=? WHERE id=?`, sid, seatID); err != nil {
		t.Fatal(err)
	}
}

// ---------- pure logic ----------

func TestGroupDiscount(t *testing.T) {
	mk := func(ev int64, sec, row string, num, price int64) Seat {
		return Seat{EventID: ev, Section: sec, Row: row, Number: num, PriceCents: price}
	}
	cases := []struct {
		name  string
		seats []Seat
		want  int64
	}{
		{"single seat", []Seat{mk(1, "F", "A", 1, 10000)}, 0},
		{"adjacent pair", []Seat{mk(1, "F", "A", 1, 10000), mk(1, "F", "A", 2, 10000)}, 2000},
		{"run of three", []Seat{mk(1, "F", "A", 1, 10000), mk(1, "F", "A", 2, 10000), mk(1, "F", "A", 3, 5000)}, 2500},
		{"gap breaks the run", []Seat{mk(1, "F", "A", 1, 10000), mk(1, "F", "A", 3, 10000)}, 0},
		{"two separate pairs", []Seat{
			mk(1, "F", "A", 1, 10000), mk(1, "F", "A", 2, 10000),
			mk(1, "F", "B", 5, 10000), mk(1, "F", "B", 6, 10000)}, 4000},
		{"different sections don't pair", []Seat{mk(1, "F", "A", 1, 10000), mk(1, "P", "A", 2, 10000)}, 0},
		{"different events don't pair", []Seat{mk(1, "F", "A", 1, 10000), mk(2, "F", "A", 2, 10000)}, 0},
	}
	for _, c := range cases {
		if got := groupDiscount(c.seats); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestTierForRow(t *testing.T) {
	// 4-row section: 1 premium, 2 standard, 1 value
	want4 := []string{"Premium", "Standard", "Standard", "Value"}
	for i, want := range want4 {
		if got := tierForRow(i, 4).name; got != want {
			t.Errorf("4-row idx %d: got %s, want %s", i, got, want)
		}
	}
	// 6-row: 2/2/2
	want6 := []string{"Premium", "Premium", "Standard", "Standard", "Value", "Value"}
	for i, want := range want6 {
		if got := tierForRow(i, 6).name; got != want {
			t.Errorf("6-row idx %d: got %s, want %s", i, got, want)
		}
	}
	// 8-row: 2/4/2
	want8 := []string{"Premium", "Premium", "Standard", "Standard", "Standard", "Standard", "Value", "Value"}
	for i, want := range want8 {
		if got := tierForRow(i, 8).name; got != want {
			t.Errorf("8-row idx %d: got %s, want %s", i, got, want)
		}
	}
}

func TestPromoValidate(t *testing.T) {
	if msg := (Promo{PercentOff: 20}).Validate(); msg != "" {
		t.Errorf("valid promo rejected: %s", msg)
	}
	if msg := (Promo{PercentOff: 20, ExpiresAt: time.Now().Add(-time.Hour)}).Validate(); msg == "" {
		t.Error("expired promo accepted")
	}
	if msg := (Promo{PercentOff: 20, MaxUses: 5, Uses: 5}).Validate(); msg == "" {
		t.Error("exhausted promo accepted")
	}
	if msg := (Promo{PercentOff: 20, ExpiresAt: time.Now().Add(time.Hour), MaxUses: 5, Uses: 4}).Validate(); msg != "" {
		t.Errorf("fresh promo under cap rejected: %s", msg)
	}
}

func TestSeatLabelAndState(t *testing.T) {
	s := Seat{Row: "B", Number: 12}
	if s.Label() != "B12" {
		t.Errorf("label: got %s", s.Label())
	}
	mine := Seat{Status: "held", HoldSession: "me", Viewer: "me"}
	if mine.SeatState() != "selected" {
		t.Errorf("mine: got %s", mine.SeatState())
	}
	theirs := Seat{Status: "held", HoldSession: "them", Viewer: "me"}
	if theirs.SeatState() != "sold" {
		t.Errorf("held-by-other should look taken: got %s", theirs.SeatState())
	}
}

// ---------- findBestBlock ----------

func TestFindBestBlock(t *testing.T) {
	setup(t)
	// custom event: Near (rank 0) and Far (rank 1), row A of 8 seats each
	if _, err := db.Exec(`INSERT INTO events (artist,title,venue,city,starts_at,description,gradient)
		VALUES ('T','T','V','C','2030-01-01T20:00:00Z','','g')`); err != nil {
		t.Fatal(err)
	}
	ev := queryInt(t, `SELECT id FROM events WHERE artist='T'`)
	seat := func(rank int64, sec, row string, num int64, status string) {
		if _, err := db.Exec(`INSERT INTO seats (event_id,section,row,number,price_cents,status,tier,stage_rank)
			VALUES (?,?,?,?,10000,?, 'Standard', ?)`, ev, sec, row, num, status, rank); err != nil {
			t.Fatal(err)
		}
	}
	// Near row A: seats 1..8, seat 1 and 5 sold → runs {2,3,4} and {6,7,8}
	for n := int64(1); n <= 8; n++ {
		st := "available"
		if n == 1 || n == 5 {
			st = "sold"
		}
		seat(0, "Near", "A", n, st)
	}
	// Far row A: fully open — a valid block exists but farther away
	for n := int64(1); n <= 8; n++ {
		seat(1, "Far", "A", n, "available")
	}

	got, err := findBestBlock(ev, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Near wins on stage rank; within it, {2,3,4} (mid 3) beats {6,7,8} (mid 7)
	// against row center 4.5
	var want []int64
	idRows, _ := db.Query(`SELECT id FROM seats WHERE event_id=? AND section='Near' AND row='A' AND number IN (2,3,4) ORDER BY number`, ev)
	for idRows.Next() {
		var id int64
		idRows.Scan(&id)
		want = append(want, id)
	}
	idRows.Close()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	// block larger than any available run → nil
	if got, err := findBestBlock(ev, 9); err != nil || got != nil {
		t.Errorf("n=9: got %v, %v — want nil", got, err)
	}
}

// ---------- handler integration ----------

func TestToggleSeatHoldsAndReleases(t *testing.T) {
	setup(t)
	seatID := queryInt(t, `SELECT id FROM seats WHERE event_id=1 AND status='available' LIMIT 1`)

	// use a real mux — handlers read path values
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat)
	req := httptest.NewRequest("POST", fmt.Sprintf("/events/1/seats/%d/toggle", seatID), nil)
	req = req.WithContext(withSession(req.Context(), "s1"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("toggle: %d", rec.Code)
	}
	if st := queryStr(t, `SELECT status FROM seats WHERE id=?`, seatID); st != "held" {
		t.Fatalf("seat not held: %s", st)
	}
	if holder := queryStr(t, `SELECT hold_session FROM seats WHERE id=?`, seatID); holder != "s1" {
		t.Fatalf("wrong holder: %s", holder)
	}
	if !strings.Contains(rec.Body.String(), "seat-selected") {
		t.Error("response should show the seat selected")
	}
	if !strings.Contains(rec.Body.String(), `id="cart"`) {
		t.Error("response should include the OOB cart update")
	}

	// toggle again releases
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if st := queryStr(t, `SELECT status FROM seats WHERE id=?`, seatID); st != "available" {
		t.Fatalf("seat not released: %s", st)
	}
}

func TestToggleCap(t *testing.T) {
	setup(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat)
	toggle := func(seatID int64, sid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", fmt.Sprintf("/events/1/seats/%d/toggle", seatID), nil)
		req = req.WithContext(withSession(req.Context(), sid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	// hold 6 distinct seats
	rows, err := db.Query(`SELECT id FROM seats WHERE event_id=1 AND status='available' LIMIT 7`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids[:6] {
		toggle(id, "capper")
	}
	// 7th is refused and stays available
	rec := toggle(ids[6], "capper")
	if st := queryStr(t, `SELECT status FROM seats WHERE id=?`, ids[6]); st != "available" {
		t.Fatalf("7th seat was held despite cap: %s", st)
	}
	if !strings.Contains(rec.Body.String(), "Limit of 6") {
		t.Error("expected cap message in cart response")
	}
}

func TestCheckoutMathGroupAndPromo(t *testing.T) {
	setup(t)
	// two adjacent seats at $100 each → subtotal $200, group −$20,
	// ENCORE20 takes 20% off the remaining $180 → $36 → total $144
	var ids []int64
	rows, err := db.Query(`SELECT s.id FROM seats s WHERE s.event_id=1 AND s.status='available'
		AND NOT EXISTS (SELECT 1 FROM seats x WHERE x.event_id=1 AND x.status='available' AND x.id=s.id-1 AND x.section=s.section AND x.row=s.row)
		AND EXISTS (SELECT 1 FROM seats x WHERE x.event_id=1 AND x.status='available' AND x.id=s.id+1 AND x.section=s.section AND x.row=s.row)
		ORDER BY s.id LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		t.Skip("no adjacent pair available in seed data")
	}
	pair := []int64{ids[0], ids[0] + 1}
	var subtotal int64
	for _, id := range pair {
		holdSeat(t, id, "buyer")
		subtotal += queryInt(t, `SELECT price_cents FROM seats WHERE id=?`, id)
	}

	rec := do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "buyer", url.Values{
		"name": {"T"}, "email": {"t@x.com"}, "card": {"4242424242424242"}, "promo": {"ENCORE20"},
	})
	if loc := rec.Header().Get("HX-Redirect"); loc == "" {
		t.Fatalf("expected redirect to order, got body %q", rec.Body.String())
	}

	oid := queryInt(t, `SELECT MAX(id) FROM orders`)
	gotTotal := queryInt(t, `SELECT total_cents FROM orders WHERE id=?`, oid)
	gotGroup := queryInt(t, `SELECT group_discount_cents FROM orders WHERE id=?`, oid)
	gotPromo := queryInt(t, `SELECT discount_cents FROM orders WHERE id=?`, oid)
	wantGroup := subtotal * 10 / 100
	wantPromo := (subtotal - wantGroup) * 20 / 100
	wantTotal := subtotal - wantGroup - wantPromo
	if gotGroup != wantGroup || gotPromo != wantPromo || gotTotal != wantTotal {
		t.Fatalf("order math: got total=%d group=%d promo=%d, want %d/%d/%d",
			gotTotal, gotGroup, gotPromo, wantTotal, wantGroup, wantPromo)
	}
	if n := queryInt(t, `SELECT COUNT(*) FROM tickets WHERE order_id=?`, oid); n != 2 {
		t.Fatalf("ticket count: %d", n)
	}
	if p := queryInt(t, `SELECT price_cents FROM tickets WHERE seat_id=?`, pair[0]); p == 0 {
		t.Error("ticket missing price snapshot")
	}
	if st := queryStr(t, `SELECT status FROM seats WHERE id=?`, pair[0]); st != "sold" {
		t.Fatalf("seat not sold: %s", st)
	}
	if u := queryInt(t, `SELECT uses FROM promos WHERE code='ENCORE20'`); u != 1 {
		t.Fatalf("promo uses: %d", u)
	}
}

func TestRefundRestoresSeatsAndPromo(t *testing.T) {
	setup(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders/{id}/refund", handleRefund)

	seatID := queryInt(t, `SELECT id FROM seats WHERE event_id=1 AND status='available' LIMIT 1`)
	holdSeat(t, seatID, "refunder")
	do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "refunder", url.Values{
		"name": {"R"}, "email": {"r@x.com"}, "card": {"4242424242424242"}, "promo": {"ENCORE20"},
	})
	oid := queryInt(t, `SELECT MAX(id) FROM orders`)

	req := httptest.NewRequest("POST", fmt.Sprintf("/orders/%d/refund", oid), nil)
	req = req.WithContext(withSession(req.Context(), "refunder"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if st := queryStr(t, `SELECT status FROM orders WHERE id=?`, oid); st != "refunded" {
		t.Fatalf("order status: %s", st)
	}
	if st := queryStr(t, `SELECT status FROM seats WHERE id=?`, seatID); st != "available" {
		t.Fatalf("seat not freed: %s", st)
	}
	if u := queryInt(t, `SELECT uses FROM promos WHERE code='ENCORE20'`); u != 0 {
		t.Fatalf("promo uses not returned: %d", u)
	}

	// other session can't refund someone else's order
	req = httptest.NewRequest("POST", fmt.Sprintf("/orders/%d/refund", oid), nil)
	req = req.WithContext(withSession(req.Context(), "intruder"))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("foreign refund should 403, got %d", rec.Code)
	}
}

func TestWaitlistOfferOnRefund(t *testing.T) {
	setup(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders/{id}/refund", handleRefund)
	mux.HandleFunc("POST /events/{id}/waitlist", handleJoinWaitlist)
	mux.HandleFunc("GET /events/{id}", handleEvent)

	// sell the event out
	if _, err := db.Exec(`UPDATE seats SET status='sold' WHERE event_id=1`); err != nil {
		t.Fatal(err)
	}
	// waiter joins
	rec := do(t, "POST /events/{id}/waitlist", handleJoinWaitlist, "POST", "/events/1/waitlist", "waiter", url.Values{"email": {"w@x.com"}})
	if !strings.Contains(rec.Body.String(), "#1 in line") {
		t.Fatalf("expected position 1, got %q", rec.Body.String())
	}
	// buyer purchases the one seat we free up
	seatID := queryInt(t, `SELECT id FROM seats WHERE event_id=1 LIMIT 1`)
	if _, err := db.Exec(`UPDATE seats SET status='available' WHERE id=?`, seatID); err != nil {
		t.Fatal(err)
	}
	holdSeat(t, seatID, "buyer")
	do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "buyer", url.Values{
		"name": {"B"}, "email": {"b@x.com"}, "card": {"4242424242424242"},
	})
	oid := queryInt(t, `SELECT MAX(id) FROM orders`)

	req := httptest.NewRequest("POST", fmt.Sprintf("/orders/%d/refund", oid), nil)
	req = req.WithContext(withSession(req.Context(), "buyer"))
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if st := queryStr(t, `SELECT status FROM waitlist WHERE session_id='waiter'`); st != "offered" {
		t.Fatalf("waitlist entry: %s", st)
	}
	if holder := queryStr(t, `SELECT hold_session FROM seats WHERE id=?`, seatID); holder != "waiter" {
		t.Fatalf("seat should be held for waiter, got %q", holder)
	}
	// the offered seat lands in the waiter's cart
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/events/1", nil).WithContext(withSession(httptest.NewRequest("GET", "/", nil).Context(), "waiter"))
	mux.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "A seat opened up") {
		t.Error("waiter should see the offer card")
	}
}

func TestOfferExpirySweep(t *testing.T) {
	setup(t)
	if _, err := db.Exec(`UPDATE seats SET status='sold' WHERE event_id=1`); err != nil {
		t.Fatal(err)
	}
	seatID := queryInt(t, `SELECT id FROM seats WHERE event_id=1 LIMIT 1`)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO waitlist (event_id,session_id,email,status,offered_seat_id,offered_at,offer_expires_at)
		VALUES (1,'stale','s@x.com','offered',?, '2020-01-01T00:00:00Z', ?)`, seatID, past); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE seats SET status='held', hold_session='stale' WHERE id=?`, seatID); err != nil {
		t.Fatal(err)
	}
	if err := sweepExpiredOffers(1); err != nil {
		t.Fatal(err)
	}
	if st := queryStr(t, `SELECT status FROM waitlist WHERE session_id='stale'`); st != "expired" {
		t.Fatalf("entry should expire, got %s", st)
	}
	if st := queryStr(t, `SELECT status FROM seats WHERE id=?`, seatID); st != "available" {
		t.Fatalf("seat should return to pool, got %s", st)
	}
}

func TestReportNumbers(t *testing.T) {
	setup(t)
	// buy two adjacent seats with promo on event 1, and one on event 2
	var pair []int64
	rows, _ := db.Query(`SELECT s.id FROM seats s WHERE s.event_id=1 AND s.status='available'
		AND EXISTS (SELECT 1 FROM seats x WHERE x.id=s.id+1 AND x.status='available' AND x.row=s.row AND x.section=s.section)
		ORDER BY s.id LIMIT 1`)
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		pair = append(pair, id, id+1)
	}
	rows.Close()
	for _, id := range pair {
		holdSeat(t, id, "rep")
	}
	do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "rep", url.Values{
		"name": {"R"}, "email": {"r@x.com"}, "card": {"4242424242424242"}, "promo": {"ENCORE20"},
	})

	v, err := buildReport(reportFilters{EventID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Rows) != 1 {
		t.Fatalf("expected 1 filtered row, got %d", len(v.Rows))
	}
	row := v.Rows[0]
	if row.Sold != 2 {
		t.Errorf("sold: got %d, want 2", row.Sold)
	}
	if row.PromoOrders != 1 {
		t.Errorf("promo orders: got %d, want 1", row.PromoOrders)
	}
	if row.NetCents <= 0 || row.NetCents >= row.GrossCents {
		t.Errorf("net %d should be positive and below gross %d", row.NetCents, row.GrossCents)
	}
	// gross should equal the two seat prices
	var gross int64
	db.QueryRow(`SELECT SUM(price_cents) FROM seats WHERE id IN (?,?)`, pair[0], pair[1]).Scan(&gross)
	if row.GrossCents != gross {
		t.Errorf("gross: got %d, want %d", row.GrossCents, gross)
	}
	// discounts = promo + group attributed across the order's seats
	if row.DiscountCents <= 0 {
		t.Error("expected attributed discounts > 0")
	}
}

// ---------- door check-in ----------

func TestDoorCodesSeeded(t *testing.T) {
	setup(t)
	if n := queryInt(t, `SELECT COUNT(*) FROM events WHERE checkin_code='' OR checkin_code IS NULL`); n != 0 {
		t.Fatalf("%d events missing door codes", n)
	}
	if n := queryInt(t, `SELECT COUNT(*) FROM (SELECT checkin_code FROM events WHERE checkin_code<>'' GROUP BY checkin_code HAVING COUNT(*)>1)`); n != 0 {
		t.Fatal("duplicate door codes")
	}
	// an event inserted without a code (the '' sentinel) must not collide
	if _, err := db.Exec(`INSERT INTO events (artist,title,venue,city,starts_at,description,gradient)
		VALUES ('X','X','V','C','2030-01-01T20:00:00Z','','g')`); err != nil {
		t.Fatalf("uncoded event insert rejected: %v", err)
	}
}

// buyTicket purchases one available seat on event 1 and returns its order and
// ticket code.
func buyTicket(t *testing.T, sid string) (orderID int64, code string) {
	t.Helper()
	seatID := queryInt(t, `SELECT id FROM seats WHERE event_id=1 AND status='available' LIMIT 1`)
	holdSeat(t, seatID, sid)
	do(t, "POST /checkout", handleCheckout, "POST", "/checkout", sid, url.Values{
		"name": {"B"}, "email": {"b@x.com"}, "card": {"4242424242424242"},
	})
	// newest ticket on the seat — a resold seat carries older refunded tickets
	return queryInt(t, `SELECT order_id FROM tickets WHERE seat_id=? ORDER BY id DESC LIMIT 1`, seatID),
		queryStr(t, `SELECT code FROM tickets WHERE seat_id=? ORDER BY id DESC LIMIT 1`, seatID)
}

func scan(t *testing.T, door, ticket string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, "POST /checkin/{doorCode}", handleCheckinScan, "POST", "/checkin/"+door, "staff", url.Values{"ticket": {ticket}})
}

func TestCheckinScan(t *testing.T) {
	setup(t)
	door := queryStr(t, `SELECT checkin_code FROM events WHERE id=1`)
	_, code := buyTicket(t, "buyer1")

	rec := scan(t, door, code)
	if rec.Code != 200 {
		t.Fatalf("first scan: %d %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Checked in") {
		t.Errorf("expected success card, got %q", rec.Body.String())
	}
	at := queryStr(t, `SELECT COALESCE(checked_in_at,'') FROM tickets WHERE code=?`, code)
	if at == "" {
		t.Fatal("checked_in_at not set")
	}
	if by := queryStr(t, `SELECT checked_in_by FROM tickets WHERE code=?`, code); by != door {
		t.Errorf("checked_in_by: got %q, want %q", by, door)
	}

	rec = scan(t, door, code)
	if rec.Code != 409 {
		t.Fatalf("second scan should be 409, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Already checked in") {
		t.Errorf("409 body should explain itself, got %q", rec.Body.String())
	}
	if at2 := queryStr(t, `SELECT checked_in_at FROM tickets WHERE code=?`, code); at2 != at {
		t.Error("rescan clobbered checked_in_at")
	}
}

func TestCheckinScanErrors(t *testing.T) {
	setup(t)
	door := queryStr(t, `SELECT checkin_code FROM events WHERE id=1`)

	if rec := scan(t, door, "TP-DEADBEEF00"); rec.Code != 404 {
		t.Errorf("unknown ticket: got %d, want 404", rec.Code)
	}
	if rec := scan(t, door, "  "); rec.Code != 400 {
		t.Errorf("empty ticket: got %d, want 400", rec.Code)
	}
	if rec := scan(t, "TPK-NOSUCHCODE00", "TP-ABC123"); rec.Code != 404 {
		t.Errorf("bad door code: got %d, want 404", rec.Code)
	}

	// a ticket for event 2 scanned at event 1's door
	seat2 := queryInt(t, `SELECT id FROM seats WHERE event_id=2 AND status='available' LIMIT 1`)
	holdSeat(t, seat2, "ev2")
	do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "ev2", url.Values{
		"name": {"W"}, "email": {"w@x.com"}, "card": {"4242424242424242"},
	})
	code2 := queryStr(t, `SELECT code FROM tickets WHERE seat_id=?`, seat2)
	if rec := scan(t, door, code2); rec.Code != 422 {
		t.Errorf("wrong-event ticket: got %d, want 422", rec.Code)
	}

	// a refunded ticket is void, not "already scanned"
	oid, code := buyTicket(t, "refunder")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders/{id}/refund", handleRefund)
	req := httptest.NewRequest("POST", fmt.Sprintf("/orders/%d/refund", oid), nil)
	req = req.WithContext(withSession(req.Context(), "refunder"))
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if rec := scan(t, door, code); rec.Code != 410 {
		t.Errorf("refunded ticket: got %d, want 410", rec.Code)
	}

	// typed the way door staff type it: lowercase, no prefix, stray spaces
	_, code3 := buyTicket(t, "typist")
	mangled := " " + strings.ToLower(strings.TrimPrefix(code3, "TP-"))
	if rec := scan(t, door, mangled); rec.Code != 200 {
		t.Errorf("normalized scan: got %d %q, want 200", rec.Code, rec.Body.String())
	}
}

func TestCheckinRefundResell(t *testing.T) {
	setup(t)
	door := queryStr(t, `SELECT checkin_code FROM events WHERE id=1`)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders/{id}/refund", handleRefund)

	oid, code := buyTicket(t, "first")
	seatID := queryInt(t, `SELECT seat_id FROM tickets WHERE code=?`, code)
	if rec := scan(t, door, code); rec.Code != 200 {
		t.Fatalf("first scan: %d", rec.Code)
	}

	req := httptest.NewRequest("POST", fmt.Sprintf("/orders/%d/refund", oid), nil)
	req = req.WithContext(withSession(req.Context(), "first"))
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if at := queryStr(t, `SELECT COALESCE(checked_in_at,'') FROM tickets WHERE code=?`, code); at != "" {
		t.Error("refund should clear checked_in_at")
	}

	// the freed seat sells again and its new ticket scans cleanly
	holdSeat(t, seatID, "second")
	do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "second", url.Values{
		"name": {"S"}, "email": {"s@x.com"}, "card": {"4242424242424242"},
	})
	code2 := queryStr(t, `SELECT code FROM tickets WHERE seat_id=? ORDER BY id DESC LIMIT 1`, seatID)
	if code2 == code {
		t.Fatal("resold seat reused the old ticket")
	}
	if rec := scan(t, door, code2); rec.Code != 200 {
		t.Fatalf("resold ticket scan: %d", rec.Code)
	}
	if rec := scan(t, door, code); rec.Code != 410 {
		t.Errorf("old ticket should be void, got %d", rec.Code)
	}

	v, err := buildReport(reportFilters{EventID: 1})
	if err != nil {
		t.Fatal(err)
	}
	row := v.Rows[0]
	if row.Sold != 1 || row.CheckedIn != 1 || row.SoldAllTime != 1 {
		t.Fatalf("report: sold=%d checkedin=%d soldall=%d, want 1/1/1", row.Sold, row.CheckedIn, row.SoldAllTime)
	}
}

func TestCheckinJSON(t *testing.T) {
	setup(t)
	door := queryStr(t, `SELECT checkin_code FROM events WHERE id=1`)
	_, code := buyTicket(t, "json")

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/checkin/"+door, strings.NewReader(url.Values{"ticket": {code}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.HandleFunc("POST /checkin/{doorCode}", handleCheckinScan)
		mux.ServeHTTP(rec, req)
		return rec
	}

	rec := post()
	if rec.Code != 200 {
		t.Fatalf("json scan: %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if body["result"] != "checked_in" || body["code"] != code {
		t.Errorf("unexpected json: %v", body)
	}

	rec = post()
	if rec.Code != 409 {
		t.Fatalf("json rescan: %d, want 409", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["result"] != "already_checked_in" {
		t.Errorf("rescan result: %v", body["result"])
	}
	if _, leaked := body["checked_in_by"]; leaked {
		t.Error("response leaks the door credential")
	}
}

func TestCheckinRotate(t *testing.T) {
	setup(t)
	old := queryStr(t, `SELECT checkin_code FROM events WHERE id=1`)
	rec := do(t, "POST /admin/events/{id}/checkin/rotate", handleCheckinRotate, "POST", "/admin/events/1/checkin/rotate", "admin", nil)
	if rec.Header().Get("HX-Redirect") == "" && rec.Code != 303 {
		t.Fatalf("rotate should redirect, got %d", rec.Code)
	}
	if cur := queryStr(t, `SELECT checkin_code FROM events WHERE id=1`); cur == old || cur == "" {
		t.Fatalf("code not rotated: %q", cur)
	}
	if rec := scan(t, old, "TP-ABC123"); rec.Code != 404 {
		t.Errorf("dead door code should 404, got %d", rec.Code)
	}
}

func TestCheckinDoorPage(t *testing.T) {
	setup(t)
	door := queryStr(t, `SELECT checkin_code FROM events WHERE id=1`)
	_, code := buyTicket(t, "doorgoer")
	scan(t, door, code)

	rec := do(t, "GET /checkin/{doorCode}", handleCheckinPage, "GET", "/checkin/"+door, "staff", nil)
	if rec.Code != 200 {
		t.Fatalf("door page: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/checkin/"+door+"/status") {
		t.Error("door page missing its status poll")
	}

	rec = do(t, "GET /checkin/{doorCode}/status", handleCheckinStatus, "GET", "/checkin/"+door+"/status", "staff", nil)
	if rec.Code != 200 {
		t.Fatalf("status fragment: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "1</span>") || !strings.Contains(rec.Body.String(), "/ 1") {
		t.Errorf("status should show 1 of 1 checked in, got %q", rec.Body.String())
	}

	rec = do(t, "GET /checkin/{doorCode}", handleCheckinPage, "GET", "/checkin/TPK-GONE", "staff", nil)
	if rec.Code != 404 {
		t.Errorf("dead door link: got %d, want 404", rec.Code)
	}
}

func TestCheckinCSVColumn(t *testing.T) {
	setup(t)
	door := queryStr(t, `SELECT checkin_code FROM events WHERE id=1`)
	_, code := buyTicket(t, "csvbuyer")
	scan(t, door, code)

	rec := do(t, "GET /admin/report.csv", handleAdminCSV, "GET", "/admin/report.csv", "", nil)
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("csv too short: %q", rec.Body.String())
	}
	head := strings.Split(lines[0], ",")
	var ci int
	for i, h := range head {
		if h == "tickets_checked_in" {
			ci = i
		}
	}
	// every row must carry the new column — a misaligned total row would
	// leave it empty or shifted
	for _, ln := range lines[1:] {
		if got := strings.Split(ln, ",")[ci]; got == "" {
			t.Errorf("missing checked_in cell in %q", ln)
		}
	}
}

func TestSessionMiddleware(t *testing.T) {
	var gotSID string
	h := sessionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSID = sessionID(r)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if gotSID == "" {
		t.Fatal("no session id injected")
	}
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "sid" && c.Value == gotSID {
			found = true
		}
	}
	if !found {
		t.Error("sid cookie not set")
	}
}
