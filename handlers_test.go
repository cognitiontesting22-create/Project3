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

// ---------- transfers & resale ----------

func getJSON(t *testing.T, target string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/tickets/"+target, nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tickets/{code}", handleTicketPage)
	mux.ServeHTTP(rec, req)
	var m map[string]any
	json.NewDecoder(rec.Body).Decode(&m)
	return rec.Code, m
}

func transferReq(t *testing.T, code, name, email string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, "POST /tickets/{code}/transfer", handleTicketTransfer, "POST", "/tickets/"+code+"/transfer", "s1",
		url.Values{"name": {name}, "email": {email}})
}

func listReq(t *testing.T, code, price string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, "POST /tickets/{code}/list", handleTicketList, "POST", "/tickets/"+code+"/list", "s1",
		url.Values{"price": {price}})
}

func delistReq(t *testing.T, code string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, "POST /tickets/{code}/delist", handleTicketDelist, "POST", "/tickets/"+code+"/delist", "s1", nil)
}

func doorCode(t *testing.T) string {
	t.Helper()
	return queryStr(t, `SELECT checkin_code FROM events WHERE id=1`)
}

func TestTransfer(t *testing.T) {
	setup(t)
	_, code := buyTicket(t, "s1")
	rec := transferReq(t, code, "Dana Lee", "dana@example.com")
	if rec.Code != 200 || rec.Header().Get("HX-Redirect") == "" {
		t.Fatalf("transfer: %d %q", rec.Code, rec.Body.String())
	}
	newCode := rec.Header().Get("HX-Redirect")[len("/tickets/"):]
	st, m := getJSON(t, newCode)
	if st != 200 || m["holder_name"] != "Dana Lee" || m["holder_email"] != "dana@example.com" || m["transfers"].(float64) != 1 {
		t.Fatalf("new code json: %d %v", st, m)
	}
	st, m = getJSON(t, code)
	if st != 410 || m["status"] != "transferred" || m["message"] != "This ticket was transferred to Dana Lee." {
		t.Fatalf("old code json: %d %v", st, m)
	}
	rec = scan(t, doorCode(t), code)
	if rec.Code != 410 || !strings.Contains(rec.Body.String(), "transferred to Dana Lee") {
		t.Fatalf("old code at door: %d %q", rec.Code, rec.Body.String())
	}
	if rec := scan(t, doorCode(t), newCode); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Dana Lee") {
		t.Fatalf("new code at door: %d %q", rec.Code, rec.Body.String())
	}
	if rec := scan(t, doorCode(t), newCode); rec.Code != 409 {
		t.Fatalf("rescan: %d", rec.Code)
	}
}

func TestTransferValidation(t *testing.T) {
	setup(t)
	_, code := buyTicket(t, "s1")
	for _, f := range [][2]string{{"", "d@x.com"}, {"D", "nope"}} {
		if rec := transferReq(t, code, f[0], f[1]); rec.Code != 422 || !strings.Contains(rec.Body.String(), "Enter a name and a valid email address") {
			t.Fatalf("validate %v: %d %q", f, rec.Code, rec.Body.String())
		}
	}
	if rec := transferReq(t, code, "X", " B@X.COM "); rec.Code != 422 || !strings.Contains(rec.Body.String(), "already belongs") {
		t.Fatalf("same email: %d %q", rec.Code, rec.Body.String())
	}
	if st, _ := getJSON(t, code); st != 200 {
		t.Fatalf("refused transfer changed ticket")
	}
}

func TestTransferRefusals(t *testing.T) {
	setup(t)
	door := doorCode(t)
	oid, code := buyTicket(t, "s1")
	scan(t, door, code)
	if rec := transferReq(t, code, "D", "d@x.com"); rec.Code != 409 || !strings.Contains(rec.Body.String(), "already been checked in") {
		t.Fatalf("checked-in transfer: %d %q", rec.Code, rec.Body.String())
	}
	do(t, "POST /orders/{id}/refund", handleRefund, "POST", fmt.Sprintf("/orders/%d/refund", oid), "s1", nil)
	if rec := transferReq(t, code, "D", "d@x.com"); rec.Code != 410 || !strings.Contains(rec.Body.String(), "refunded and cannot be transferred") {
		t.Fatalf("refunded transfer: %d %q", rec.Code, rec.Body.String())
	}
	_, c2 := buyTicket(t, "s1")
	r := transferReq(t, c2, "Dana", "d@x.com")
	c3 := r.Header().Get("HX-Redirect")[len("/tickets/"):]
	r = transferReq(t, c3, "Eli", "e@x.com")
	c4 := r.Header().Get("HX-Redirect")[len("/tickets/"):]
	r = transferReq(t, c4, "Fay", "f@x.com")
	c5 := r.Header().Get("HX-Redirect")[len("/tickets/"):]
	if rec := transferReq(t, c5, "Gus", "g@x.com"); rec.Code != 409 || !strings.Contains(rec.Body.String(), "limit of 3 transfers") {
		t.Fatalf("limit: %d %q", rec.Code, rec.Body.String())
	}
	_, m := getJSON(t, c5)
	if m["transfers"].(float64) != 3 {
		t.Fatalf("transfers: %v", m["transfers"])
	}
	h := m["holders"].([]any)
	if len(h) != 4 || h[0].(map[string]any)["name"] != "B" || h[3].(map[string]any)["name"] != "Fay" {
		t.Fatalf("chain: %v", h)
	}
	if st, m := getJSON(t, c4); st != 410 || m["message"] != "This ticket was transferred to Fay." {
		t.Fatalf("dead code: %d %v", st, m)
	}
	if rec := scan(t, door, c5); rec.Code != 200 {
		t.Fatalf("limited code still scans: %d", rec.Code)
	}
	if rec := transferReq(t, "TP-DEADBEEF", "X", "x@x.com"); rec.Code != 404 {
		t.Fatalf("unknown: %d", rec.Code)
	}
	if rec := transferReq(t, code, "D", "d@x.com"); rec.Code != 410 {
		t.Fatalf("order of checks: refunded should beat checked-in, got %d", rec.Code)
	}
}

func TestListingCap(t *testing.T) {
	setup(t)
	_, code := buyTicket(t, "s1")
	face := queryInt(t, `SELECT price_cents FROM tickets WHERE code=?`, code)
	cap := face * 110 / 100
	for _, p := range []string{"abc", "130.005", "-5", "0"} {
		if rec := listReq(t, code, p); rec.Code != 422 || !strings.Contains(rec.Body.String(), "Enter a resale price in dollars") {
			t.Fatalf("price %q: %d %q", p, rec.Code, rec.Body.String())
		}
	}
	want := fmt.Sprintf("Resale price cannot be more than $%.2f, which is 110%% of the $%.2f face value.", float64(cap)/100, float64(face)/100)
	over := float64(cap)/100 + 0.01
	if rec := listReq(t, code, fmt.Sprintf("%.2f", over)); rec.Code != 422 || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("cap: %d %q", rec.Code, rec.Body.String())
	}
	if rec := listReq(t, code, fmt.Sprintf("%.2f", float64(cap)/100)); rec.Header().Get("HX-Redirect") == "" {
		t.Fatalf("at cap refused: %d %q", rec.Code, rec.Body.String())
	}
	st, m := getJSON(t, code)
	if st != 200 || m["status"] != "listed" || m["listing_price_cents"].(float64) != float64(cap) {
		t.Fatalf("listed json: %d %v", st, m)
	}
	if _, err := db.Exec(`UPDATE tickets SET listing_price_cents=0, price_cents=4900 WHERE code=?`, code); err != nil {
		t.Fatal(err)
	}
	want = "Resale price cannot be more than $53.90, which is 110% of the $49.00 face value."
	if rec := listReq(t, code, "53.91"); rec.Code != 422 || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("cap2: %d %q", rec.Code, rec.Body.String())
	}
	if rec := listReq(t, code, "53.90"); rec.Header().Get("HX-Redirect") == "" {
		t.Fatalf("at cap2 refused: %d %q", rec.Code, rec.Body.String())
	}
}

func TestDelistAndTransferWhileListed(t *testing.T) {
	setup(t)
	_, code := buyTicket(t, "s1")
	listReq(t, code, "120.00")
	if rec := transferReq(t, code, "D", "d@x.com"); rec.Code != 409 || !strings.Contains(rec.Body.String(), "listed for resale. Delist it before transferring") {
		t.Fatalf("transfer while listed: %d %q", rec.Code, rec.Body.String())
	}
	if rec := delistReq(t, code); rec.Header().Get("HX-Redirect") == "" {
		t.Fatalf("delist: %d %q", rec.Code, rec.Body.String())
	}
	if rec := delistReq(t, code); rec.Code != 409 || !strings.Contains(rec.Body.String(), "not listed for resale") {
		t.Fatalf("delist unlisted: %d %q", rec.Code, rec.Body.String())
	}
	if rec := transferReq(t, code, "D", "d@x.com"); rec.Header().Get("HX-Redirect") == "" {
		t.Fatalf("transfer after delist: %d", rec.Code)
	}
}

func TestResalePurchase(t *testing.T) {
	setup(t)
	oid, code := buyTicket(t, "s1")
	seatID := queryInt(t, `SELECT seat_id FROM tickets WHERE code=?`, code)
	listReq(t, code, "130.00")
	// seller cannot buy own listing
	rec := do(t, "POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat, "POST", fmt.Sprintf("/events/1/seats/%d/toggle", seatID), "s1", nil)
	if !strings.Contains(rec.Body.String(), "cannot buy your own resale listing") {
		t.Fatalf("own listing: %q", rec.Body.String())
	}
	// buyer selects and checks out through the real routes
	rec = do(t, "POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat, "POST", fmt.Sprintf("/events/1/seats/%d/toggle", seatID), "s2", nil)
	if st := queryInt(t, `SELECT COUNT(*) FROM seats WHERE id=? AND status='held' AND hold_session='s2'`, seatID); st != 1 {
		t.Fatalf("resale seat not held")
	}
	rec = do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "s2", url.Values{
		"name": {"Buyer"}, "email": {"buyer@x.com"}, "card": {"4242424242424242"}})
	newOrder := rec.Header().Get("HX-Redirect")
	if newOrder == "" {
		t.Fatalf("checkout: %d %q", rec.Code, rec.Body.String())
	}
	newCode := queryStr(t, `SELECT code FROM tickets WHERE seat_id=? AND kind='resale'`, seatID)
	face := queryInt(t, `SELECT price_cents FROM seats WHERE id=?`, seatID)
	st, m := getJSON(t, newCode)
	if st != 200 || m["holder_name"] != "Buyer" || m["transfers"].(float64) != 0 || m["face_value_cents"].(float64) != float64(face) {
		t.Fatalf("buyer ticket: %d %v", st, m)
	}
	if h := m["holders"].([]any); len(h) != 1 {
		t.Fatalf("buyer chain: %v", h)
	}
	st, m = getJSON(t, code)
	if st != 410 || m["status"] != "resold" || m["resale_price_cents"].(float64) != 13000 ||
		m["fee_cents"].(float64) != 650 || m["payout_cents"].(float64) != 12350 {
		t.Fatalf("seller code: %d %v", st, m)
	}
	rec = scan(t, doorCode(t), code)
	if rec.Code != 410 || !strings.Contains(rec.Body.String(), "sold on resale") {
		t.Fatalf("seller code at door: %d %q", rec.Code, rec.Body.String())
	}
	if rec := scan(t, doorCode(t), newCode); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Buyer") {
		t.Fatalf("buyer code at door: %d", rec.Code)
	}
	if n := queryInt(t, `SELECT COUNT(*) FROM resales WHERE ticket_id=(SELECT id FROM tickets WHERE code=?) AND price_cents=13000 AND fee_cents=650 AND payout_cents=12350`, code); n != 1 {
		t.Fatal("resale row missing")
	}
	_ = oid
}

func TestResaleFees(t *testing.T) {
	for _, c := range [][3]int64{{13000, 650, 12350}, {13750, 688, 13062}, {5390, 270, 5120}} {
		fee := (c[0]*5 + 50) / 100
		if fee != c[1] || c[0]-fee != c[2] {
			t.Fatalf("price %d: fee %d payout %d", c[0], fee, c[0]-fee)
		}
	}
}

func TestResaleHoldReleased(t *testing.T) {
	setup(t)
	_, code := buyTicket(t, "s1")
	seatID := queryInt(t, `SELECT seat_id FROM tickets WHERE code=?`, code)
	listReq(t, code, "130.00")
	do(t, "POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat, "POST", fmt.Sprintf("/events/1/seats/%d/toggle", seatID), "s2", nil)
	// held: delist refused, then unselect returns to listed
	if rec := delistReq(t, code); rec.Code != 409 || !strings.Contains(rec.Body.String(), "in a buyer's cart") {
		t.Fatalf("delist while held: %d %q", rec.Code, rec.Body.String())
	}
	do(t, "POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat, "POST", fmt.Sprintf("/events/1/seats/%d/toggle", seatID), "s2", nil)
	st, m := getJSON(t, code)
	if st != 200 || m["status"] != "listed" || queryInt(t, `SELECT COUNT(*) FROM seats WHERE id=? AND status='sold'`, seatID) != 1 {
		t.Fatalf("released: %d %v seat not back to listed", st, m)
	}
}

func TestResaleWaitlist(t *testing.T) {
	setup(t)
	_, code := buyTicket(t, "s1")
	seatID := queryInt(t, `SELECT seat_id FROM tickets WHERE code=?`, code)
	if _, err := db.Exec(`UPDATE seats SET status='sold' WHERE event_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE seats SET status='sold' WHERE id=?`, seatID); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO waitlist (event_id,session_id,email) VALUES (1,'w1','w1@x'),(1,'w2','w2@x')`)
	listReq(t, code, "130.00")
	if n := queryInt(t, `SELECT COUNT(*) FROM waitlist WHERE session_id='w1' AND status='offered' AND offered_seat_id=?`, seatID); n != 1 {
		t.Fatal("listing did not go to first waiter")
	}
	if st := queryInt(t, `SELECT COUNT(*) FROM seats WHERE id=? AND status='held' AND hold_session='w1'`, seatID); st != 1 {
		t.Fatal("seat not held for waiter")
	}
	if rec := delistReq(t, code); rec.Code != 409 {
		t.Fatalf("delist during offer: %d", rec.Code)
	}
	db.Exec(`UPDATE waitlist SET offer_expires_at='2000-01-01T00:00:00Z' WHERE status='offered'`)
	if err := sweepExpiredOffers(1); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, `SELECT COUNT(*) FROM waitlist WHERE session_id='w2' AND status='offered' AND offered_seat_id=?`, seatID); n != 1 {
		t.Fatal("expired offer did not move to next waiter")
	}
}

func TestReportResaleCols(t *testing.T) {
	setup(t)
	_, c1 := buyTicket(t, "s1")
	r := transferReq(t, c1, "Dana", "d@x.com")
	c2 := r.Header().Get("HX-Redirect")[len("/tickets/"):]
	transferReq(t, c2, "Eli", "e@x.com")
	oid2, c3 := buyTicket(t, "s1")
	seat3 := queryInt(t, `SELECT seat_id FROM tickets WHERE code=?`, c3)
	listReq(t, c3, "130.00")
	do(t, "POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat, "POST", fmt.Sprintf("/events/1/seats/%d/toggle", seat3), "s2", nil)
	do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "s2", url.Values{
		"name": {"B2"}, "email": {"b2@x.com"}, "card": {"4242424242424242"}})
	rec := do(t, "GET /admin/report.csv", handleAdminCSV, "GET", "/admin/report.csv", "", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "transfers,resale_count,resale_volume,resale_fees") {
		t.Fatalf("csv header: %q", body)
	}
	found := false
	for _, ln := range strings.Split(body, "\n") {
		if strings.Contains(ln, "Nova Reign") && strings.Contains(ln, ",2,1,130.00,6.50") {
			found = true
		}
	}
	if !found {
		t.Fatalf("csv row: %q", body)
	}
	v, err := buildReport(reportFilters{})
	if err != nil {
		t.Fatal(err)
	}
	var row ReportRow
	for _, rr := range v.Rows {
		if rr.EventID == 1 {
			row = rr
		}
	}
	if row.Transfers != 2 || row.ResaleCount != 1 || row.ResaleVolumeCents != 13000 || row.ResaleFeesCents != 650 {
		t.Fatalf("report: %+v", row)
	}
	_ = oid2
}

func TestPromoAndGroupDealOnResale(t *testing.T) {
	setup(t)
	db.Exec(`INSERT INTO promos (code,percent_off) VALUES ('ENCORE20',20)`)
	_, code := buyTicket(t, "s1")
	seatR := queryInt(t, `SELECT seat_id FROM tickets WHERE code=?`, code)
	listReq(t, code, "130.00")
	var n4, n5 int64
	db.QueryRow(`SELECT s1.id, s2.id FROM seats s1 JOIN seats s2 ON s2.event_id=s1.event_id AND s2.section=s1.section AND s2.row=s1.row AND s2.number=s1.number+1
		WHERE s1.event_id=1 AND s1.status='available' AND s2.status='available' AND s1.section='Floor'
		AND s1.price_cents=12500 AND s2.price_cents=12500 AND s1.id<>? AND s2.id<>?`, seatR, seatR).Scan(&n4, &n5)
	holdSeat(t, n4, "s2")
	holdSeat(t, n5, "s2")
	do(t, "POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat, "POST", fmt.Sprintf("/events/1/seats/%d/toggle", seatR), "s2", nil)
	rec := do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "s2", url.Values{
		"name": {"B"}, "email": {"b@x.com"}, "card": {"4242424242424242"}, "promo": {"ENCORE20"}})
	newOrder := rec.Header().Get("HX-Redirect")[len("/orders/"):]
	var total, disc, grp int64
	var promo string
	db.QueryRow(`SELECT total_cents,discount_cents,group_discount_cents,COALESCE(promo_code,'') FROM orders WHERE id=?`, newOrder).Scan(&total, &disc, &grp, &promo)
	if grp != 2500 || disc != 4500 || total != 31000 || promo != "ENCORE20" {
		t.Fatalf("mixed order: grp %d disc %d total %d promo %q", grp, disc, total, promo)
	}
	// resale-only cart: promo not applied, uses unchanged
	_, c2 := buyTicket(t, "s1")
	seatR2 := queryInt(t, `SELECT seat_id FROM tickets WHERE code=?`, c2)
	listReq(t, c2, "100.00")
	do(t, "POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat, "POST", fmt.Sprintf("/events/1/seats/%d/toggle", seatR2), "s3", nil)
	rec = do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "s3", url.Values{
		"name": {"B"}, "email": {"b@x.com"}, "card": {"4242424242424242"}, "promo": {"ENCORE20"}})
	newOrder = rec.Header().Get("HX-Redirect")[len("/orders/"):]
	db.QueryRow(`SELECT total_cents,discount_cents,COALESCE(promo_code,'') FROM orders WHERE id=?`, newOrder).Scan(&total, &disc, &promo)
	if total != 10000 || disc != 0 || promo != "" {
		t.Fatalf("resale-only: total %d disc %d promo %q", total, disc, promo)
	}
	if n := queryInt(t, `SELECT uses FROM promos WHERE code='ENCORE20'`); n != 1 {
		t.Fatalf("promo uses moved: %d", n)
	}
}

func TestRefundRefusals(t *testing.T) {
	setup(t)
	oid1, c1 := buyTicket(t, "s1")
	transferReq(t, c1, "Dana", "d@x.com")
	rec := do(t, "POST /orders/{id}/refund", handleRefund, "POST", fmt.Sprintf("/orders/%d/refund", oid1), "s1", nil)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "transferred or resold") {
		t.Fatalf("transferred refund: %d %q", rec.Code, rec.Body.String())
	}
	oid2, c2 := buyTicket(t, "s1")
	listReq(t, c2, "120.00")
	rec = do(t, "POST /orders/{id}/refund", handleRefund, "POST", fmt.Sprintf("/orders/%d/refund", oid2), "s1", nil)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "listed for resale. Delist it before refunding") {
		t.Fatalf("listed refund: %d %q", rec.Code, rec.Body.String())
	}
	seat2 := queryInt(t, `SELECT seat_id FROM tickets WHERE code=?`, c2)
	do(t, "POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat, "POST", fmt.Sprintf("/events/1/seats/%d/toggle", seat2), "s2", nil)
	rec = do(t, "POST /checkout", handleCheckout, "POST", "/checkout", "s2", url.Values{
		"name": {"B"}, "email": {"b@x.com"}, "card": {"4242424242424242"}})
	resaleOrder := rec.Header().Get("HX-Redirect")[len("/orders/"):]
	rec = do(t, "POST /orders/{id}/refund", handleRefund, "POST", "/orders/"+resaleOrder+"/refund", "s2", nil)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "Resale purchases are final") {
		t.Fatalf("resale refund: %d %q", rec.Code, rec.Body.String())
	}
	if st := queryInt(t, `SELECT COUNT(*) FROM orders WHERE id=? AND status='paid'`, oid2); st != 1 {
		t.Fatal("refusal changed the order")
	}
}
