package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ctxKey int

const sessionKey ctxKey = 0

func withSession(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, sessionKey, sid)
}

func sessionID(r *http.Request) string {
	sid, _ := r.Context().Value(sessionKey).(string)
	return sid
}

type Event struct {
	ID            int64
	Artist        string
	Title         string
	Venue         string
	City          string
	StartsAt      string
	Description   string
	Gradient      string
	MinPriceCents int64
	SoldOut       bool
}

type Seat struct {
	ID          int64
	EventID     int64
	Section     string
	Row         string
	Number      int64
	PriceCents  int64
	Status      string // raw status: available | held | sold
	HoldSession string
	Viewer      string // requesting session, used to render state
	EventName   string // "Artist — Title", populated for cart/checkout views
}

// Mine reports whether the current session holds this seat.
func (s Seat) Mine() bool {
	return s.Status == "held" && s.HoldSession == s.Viewer
}

// Label is a short human label like "B12".
func (s Seat) Label() string { return s.Row + strconv.FormatInt(s.Number, 10) }

// SeatState resolves how to render the seat for the requesting session.
func (s Seat) SeatState() string {
	switch {
	case s.Status == "sold":
		return "sold"
	case s.Mine():
		return "selected"
	case s.Status == "held":
		return "sold" // held by someone else looks taken
	default:
		return "available"
	}
}

type SeatRow struct {
	Label string
	Seats []Seat
}

type SeatSection struct {
	Name       string
	PriceCents int64
	Rows       []SeatRow
}

const maxTicketsPerEvent = 6

type CartView struct {
	EventID    int64
	Seats      []Seat
	TotalCents int64
	Used       int64 // seats already owned + currently held for this event
	Max        int64
	Err        string
}

type eventPage struct {
	Event    Event
	Sections []SeatSection
	Cart     CartView
	Promo    *Promo
	Waitlist *WaitlistView
}

const offerTTL = 15 * time.Minute

// WaitlistView is the sidebar card for a sold-out event.
// State is "join", "waiting" or "offered"; empty means show nothing.
type WaitlistView struct {
	EventID      int64
	State        string
	Position     int64
	Email        string
	SeatLabel    string
	MinutesLeft  int64
	Err          string
}

// Promo is a percent-off discount code with optional expiry and usage cap.
type Promo struct {
	ID         int64
	Code       string
	PercentOff int64
	ExpiresAt  time.Time // zero = no expiry
	MaxUses    int64     // 0 = unlimited
	Uses       int64
}

// Validate returns a user-facing reason the code can't be used, or "".
func (p Promo) Validate() string {
	switch {
	case !p.ExpiresAt.IsZero() && time.Now().After(p.ExpiresAt):
		return "That promo code has expired."
	case p.MaxUses > 0 && p.Uses >= p.MaxUses:
		return "That promo code has reached its usage limit."
	default:
		return ""
	}
}

func (p Promo) ExpiresLabel() string {
	if p.ExpiresAt.IsZero() {
		return ""
	}
	return p.ExpiresAt.Format("Jan 2")
}

type Order struct {
	ID            int64
	SessionID     string
	Name          string
	Email         string
	TotalCents    int64
	DiscountCents int64
	PromoCode     string
	Status        string
	RefundedAt    string
	CreatedAt     string
}

type Ticket struct {
	ID       int64
	Code     string
	Section  string
	Row      string
	Number   int64
	EventID  int64
	Artist   string
	Title    string
	Venue    string
	City     string
	StartsAt string
	Gradient string
}

// ---------- handlers ----------

func handleIndex(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`
		SELECT e.id, e.artist, e.title, e.venue, e.city, e.starts_at, e.description, e.gradient,
		       COALESCE((SELECT MIN(price_cents) FROM seats s WHERE s.event_id = e.id AND s.status='available'), 0),
		       NOT EXISTS (SELECT 1 FROM seats s WHERE s.event_id = e.id AND s.status='available')
		FROM events e ORDER BY e.starts_at`)
	if err != nil {
		httpErr(w, err)
		return
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Artist, &e.Title, &e.Venue, &e.City, &e.StartsAt, &e.Description, &e.Gradient, &e.MinPriceCents, &e.SoldOut); err != nil {
			httpErr(w, err)
			return
		}
		events = append(events, e)
	}
	render(w, "events.html", struct {
		Events []Event
		Promo  *Promo
	}{events, promoForSession(sessionID(r))})
}

func handleEvent(w http.ResponseWriter, r *http.Request) {
	eventID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	e, err := getEvent(eventID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sid := sessionID(r)
	if err := sweepExpiredOffers(eventID); err != nil {
		httpErr(w, err)
		return
	}
	sections, err := getSections(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	cart, err := getCart(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	wl, err := getWaitlistView(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	render(w, "event.html", eventPage{Event: e, Sections: sections, Cart: cart, Promo: promoForSession(sid), Waitlist: wl})
}

func handleToggleSeat(w http.ResponseWriter, r *http.Request) {
	eventID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	seatID, _ := strconv.ParseInt(r.PathValue("seatID"), 10, 64)
	sid := sessionID(r)

	tx, err := db.Begin()
	if err != nil {
		httpErr(w, err)
		return
	}
	defer tx.Rollback()

	var seat Seat
	var holder sql.NullString
	err = tx.QueryRow(`SELECT id,event_id,section,row,number,price_cents,status,hold_session
		FROM seats WHERE id=? AND event_id=?`, seatID, eventID).
		Scan(&seat.ID, &seat.EventID, &seat.Section, &seat.Row, &seat.Number, &seat.PriceCents, &seat.Status, &holder)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	seat.HoldSession = holder.String

	seat.Viewer = sid
	var capErr string
	switch {
	case seat.Status == "available":
		owned, held, err := ticketUsage(eventID, sid)
		if err != nil {
			httpErr(w, err)
			return
		}
		if owned+held >= maxTicketsPerEvent {
			capErr = fmt.Sprintf("Limit of %d tickets per event — you've already got %d.", maxTicketsPerEvent, owned+held)
			break
		}
		if _, err := tx.Exec(`UPDATE seats SET status='held', hold_session=? WHERE id=?`, sid, seat.ID); err != nil {
			httpErr(w, err)
			return
		}
		seat.Status = "held"
		seat.HoldSession = sid
	case seat.Mine():
		if _, err := tx.Exec(`UPDATE seats SET status='available', hold_session=NULL WHERE id=?`, seat.ID); err != nil {
			httpErr(w, err)
			return
		}
		// a released seat goes to the next person in line, if anyone's waiting
		if err := releaseSeatOffers(tx, seat.ID, eventID); err != nil {
			httpErr(w, err)
			return
		}
		if err := tx.QueryRow(`SELECT status, COALESCE(hold_session,'') FROM seats WHERE id=?`, seat.ID).
			Scan(&seat.Status, &seat.HoldSession); err != nil {
			httpErr(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpErr(w, err)
		return
	}

	cart, err := getCart(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	cart.Err = capErr
	render(w, "seat.html", struct {
		Seat Seat
		Cart CartView
	}{seat, cart})
}

func handleClearCart(w http.ResponseWriter, r *http.Request) {
	eventID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sid := sessionID(r)
	tx, err := db.Begin()
	if err != nil {
		httpErr(w, err)
		return
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM seats WHERE event_id=? AND status='held' AND hold_session=?`, eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	var released []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			httpErr(w, err)
			return
		}
		released = append(released, id)
	}
	rows.Close()
	for _, seatID := range released {
		if _, err := tx.Exec(`UPDATE seats SET status='available', hold_session=NULL WHERE id=?`, seatID); err != nil {
			httpErr(w, err)
			return
		}
		if err := releaseSeatOffers(tx, seatID, eventID); err != nil {
			httpErr(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpErr(w, err)
		return
	}
	cart, err := getCart(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	sections, err := getSections(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	render(w, "clearresp.html", struct {
		Sections []SeatSection
		Cart     CartView
	}{sections, cart})
}

type checkoutView struct {
	Event         Event
	Seats         []Seat
	SubtotalCents int64
	DiscountCents int64
	TotalCents    int64
	PromoCode     string
	PromoErr      string
	Err           string
}

func buildCheckoutView(sid, promoCode string) (checkoutView, error) {
	if err := sweepForCart(sid); err != nil {
		return checkoutView{}, err
	}
	seats, err := heldSeats(sid)
	if err != nil {
		return checkoutView{}, err
	}
	if len(seats) == 0 {
		return checkoutView{}, errEmptyCart
	}
	e, err := getEvent(seats[0].EventID)
	if err != nil {
		return checkoutView{}, err
	}
	v := checkoutView{Event: e, Seats: seats, PromoCode: promoCode}
	for _, s := range seats {
		v.SubtotalCents += s.PriceCents
	}
	if promoCode != "" {
		p, err := promoByCode(promoCode)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			v.PromoErr = "Unknown promo code."
		case err != nil:
			return checkoutView{}, err
		default:
			if msg := p.Validate(); msg != "" {
				v.PromoErr = msg
			} else {
				v.DiscountCents = v.SubtotalCents * p.PercentOff / 100
			}
		}
	}
	v.TotalCents = v.SubtotalCents - v.DiscountCents
	return v, nil
}

var errEmptyCart = errors.New("no seats held")

func handleCheckoutPage(w http.ResponseWriter, r *http.Request) {
	v, err := buildCheckoutView(sessionID(r), "")
	if errors.Is(err, errEmptyCart) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	render(w, "checkout.html", v)
}

// handleApplyPromo re-renders the order-summary fragment with the code applied.
func handleApplyPromo(w http.ResponseWriter, r *http.Request) {
	sid := sessionID(r)
	if err := r.ParseForm(); err != nil {
		httpErr(w, err)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(r.FormValue("promo")))
	v, err := buildCheckoutView(sid, code)
	if errors.Is(err, errEmptyCart) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	render(w, "summary.html", v)
}

func handleCheckout(w http.ResponseWriter, r *http.Request) {
	sid := sessionID(r)
	if err := r.ParseForm(); err != nil {
		httpErr(w, err)
		return
	}
	if err := sweepForCart(sid); err != nil {
		httpErr(w, err)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	email := strings.TrimSpace(r.FormValue("email"))
	card := strings.ReplaceAll(strings.TrimSpace(r.FormValue("card")), " ", "")
	promoInput := strings.ToUpper(strings.TrimSpace(r.FormValue("promo")))

	fail := func(msg string) {
		render(w, "payerr.html", struct{ Err string }{msg})
	}

	if name == "" || !strings.Contains(email, "@") {
		fail("Please enter your name and a valid email address.")
		return
	}
	if len(card) != 16 {
		fail("Card number must be 16 digits. This is a demo — try 4242 4242 4242 4242.")
		return
	}
	for _, c := range card {
		if c < '0' || c > '9' {
			fail("Card number must be digits only.")
			return
		}
	}
	if card == "4000000000000002" {
		fail("Card declined. Try 4242 4242 4242 4242 — every other number works here.")
		return
	}

	tx, err := db.Begin()
	if err != nil {
		httpErr(w, err)
		return
	}
	defer tx.Rollback()

	// Confirm the seats are still held by this session before charging.
	rows, err := tx.Query(`SELECT id, price_cents, event_id FROM seats WHERE status='held' AND hold_session=?`, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	var seatIDs []int64
	var eventID int64
	var subtotal int64
	for rows.Next() {
		var id, price, ev int64
		if err := rows.Scan(&id, &price, &ev); err != nil {
			rows.Close()
			httpErr(w, err)
			return
		}
		seatIDs = append(seatIDs, id)
		eventID = ev
		subtotal += price
	}
	rows.Close()
	if len(seatIDs) == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// re-verify the per-event cap against owned tickets + this cart
	var owned int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tickets t
		JOIN orders o ON o.id = t.order_id
		JOIN seats s ON s.id = t.seat_id
		WHERE o.session_id = ? AND s.event_id = ?`, sid, eventID).Scan(&owned); err != nil {
		httpErr(w, err)
		return
	}
	if owned+int64(len(seatIDs)) > maxTicketsPerEvent {
		fail(fmt.Sprintf("Limit of %d tickets per event — you already have %d.", maxTicketsPerEvent, owned))
		return
	}

	var discount int64
	var appliedCode string
	if promoInput != "" {
		var p Promo
		var exp sql.NullString
		var max sql.NullInt64
		err := tx.QueryRow(`SELECT id,code,percent_off,expires_at,max_uses,uses FROM promos WHERE code=?`, promoInput).
			Scan(&p.ID, &p.Code, &p.PercentOff, &exp, &max, &p.Uses)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			fail("Unknown promo code.")
			return
		case err != nil:
			httpErr(w, err)
			return
		}
		if exp.Valid && exp.String != "" {
			p.ExpiresAt, _ = time.Parse(time.RFC3339, exp.String)
		}
		if max.Valid {
			p.MaxUses = max.Int64
		}
		if msg := p.Validate(); msg != "" {
			fail(msg)
			return
		}
		res, err := tx.Exec(`UPDATE promos SET uses = uses + 1 WHERE id = ? AND (max_uses IS NULL OR uses < max_uses)`, p.ID)
		if err != nil {
			httpErr(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fail("That promo code has reached its usage limit.")
			return
		}
		discount = subtotal * p.PercentOff / 100
		appliedCode = p.Code
	}
	total := subtotal - discount

	res, err := tx.Exec(`INSERT INTO orders (session_id,name,email,total_cents,promo_code,discount_cents) VALUES (?,?,?,?,?,?)`,
		sid, name, email, total, appliedCode, discount)
	if err != nil {
		httpErr(w, err)
		return
	}
	orderID, _ := res.LastInsertId()

	for _, seatID := range seatIDs {
		if _, err := tx.Exec(`UPDATE seats SET status='sold', hold_session=NULL WHERE id=? AND status='held' AND hold_session=?`, seatID, sid); err != nil {
			httpErr(w, err)
			return
		}
		if _, err := tx.Exec(`UPDATE waitlist SET status='claimed' WHERE offered_seat_id=? AND status='offered' AND session_id=?`, seatID, sid); err != nil {
			httpErr(w, err)
			return
		}
		if _, err := tx.Exec(`INSERT INTO tickets (order_id, seat_id, code) VALUES (?,?,?)`, orderID, seatID, "TP-"+strings.ToUpper(newID(5))); err != nil {
			httpErr(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpErr(w, err)
		return
	}

	url := fmt.Sprintf("/orders/%d", orderID)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", url)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func handleOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var o Order
	var refundedAt sql.NullString
	err = db.QueryRow(`SELECT id,session_id,name,email,total_cents,discount_cents,promo_code,status,refunded_at,created_at FROM orders WHERE id=?`, orderID).
		Scan(&o.ID, &o.SessionID, &o.Name, &o.Email, &o.TotalCents, &o.DiscountCents, &o.PromoCode, &o.Status, &refundedAt, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	rows, err := db.Query(`
		SELECT t.id, t.code, s.section, s.row, s.number,
		       e.id, e.artist, e.title, e.venue, e.city, e.starts_at, e.gradient
		FROM tickets t
		JOIN seats s ON s.id = t.seat_id
		JOIN events e ON e.id = s.event_id
		WHERE t.order_id = ?
		ORDER BY s.section, s.row, s.number`, orderID)
	if err != nil {
		httpErr(w, err)
		return
	}
	defer rows.Close()
	var tickets []Ticket
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.Code, &t.Section, &t.Row, &t.Number,
			&t.EventID, &t.Artist, &t.Title, &t.Venue, &t.City, &t.StartsAt, &t.Gradient); err != nil {
			httpErr(w, err)
			return
		}
		tickets = append(tickets, t)
	}
	o.RefundedAt = refundedAt.String
	render(w, "order.html", struct {
		Order   Order
		Tickets []Ticket
		IsOwner bool
	}{o, tickets, o.SessionID == sessionID(r)})
}

// handleRefund cancels a paid order: every seat is freed (and offered to the
// waitlist first), and a promo use is returned so someone else can claim it.
func handleRefund(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sid := sessionID(r)

	tx, err := db.Begin()
	if err != nil {
		httpErr(w, err)
		return
	}
	defer tx.Rollback()

	var ownerSid, status, promo string
	err = tx.QueryRow(`SELECT session_id, status, COALESCE(promo_code,'') FROM orders WHERE id=?`, orderID).
		Scan(&ownerSid, &status, &promo)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	if ownerSid != sid {
		http.Error(w, "this isn't your order", http.StatusForbidden)
		return
	}
	url := fmt.Sprintf("/orders/%d", orderID)
	if status != "paid" {
		http.Redirect(w, r, url, http.StatusSeeOther)
		return
	}

	if _, err := tx.Exec(`UPDATE orders SET status='refunded', refunded_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`, orderID); err != nil {
		httpErr(w, err)
		return
	}

	rows, err := tx.Query(`SELECT t.seat_id, s.event_id FROM tickets t JOIN seats s ON s.id = t.seat_id WHERE t.order_id=?`, orderID)
	if err != nil {
		httpErr(w, err)
		return
	}
	type freedSeat struct{ seatID, eventID int64 }
	var freed []freedSeat
	for rows.Next() {
		var f freedSeat
		if err := rows.Scan(&f.seatID, &f.eventID); err != nil {
			rows.Close()
			httpErr(w, err)
			return
		}
		freed = append(freed, f)
	}
	rows.Close()

	for _, f := range freed {
		if _, err := tx.Exec(`UPDATE seats SET status='available', hold_session=NULL WHERE id=?`, f.seatID); err != nil {
			httpErr(w, err)
			return
		}
		if err := offerSeat(tx, f.seatID, f.eventID); err != nil {
			httpErr(w, err)
			return
		}
	}
	if promo != "" {
		if _, err := tx.Exec(`UPDATE promos SET uses = MAX(uses - 1, 0) WHERE code = ?`, promo); err != nil {
			httpErr(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpErr(w, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", url)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func handleJoinWaitlist(w http.ResponseWriter, r *http.Request) {
	eventID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sid := sessionID(r)
	email := strings.TrimSpace(r.FormValue("email"))

	fail := func(msg string) {
		v, _ := getWaitlistView(eventID, sid)
		if v == nil {
			v = &WaitlistView{EventID: eventID, State: "join"}
		}
		v.Err = msg
		render(w, "waitlistcard.html", v)
	}

	if err := sweepExpiredOffers(eventID); err != nil {
		httpErr(w, err)
		return
	}

	if !strings.Contains(email, "@") {
		fail("Enter an email so we can reach you when a seat opens up.")
		return
	}
	var existing int64
	err := db.QueryRow(`SELECT id FROM waitlist WHERE event_id=? AND session_id=? AND status IN ('waiting','offered')`, eventID, sid).Scan(&existing)
	if err == nil {
		v, err := getWaitlistView(eventID, sid)
		if err != nil {
			httpErr(w, err)
			return
		}
		render(w, "waitlistcard.html", v)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		httpErr(w, err)
		return
	}
	soldOut, err := eventSoldOut(eventID)
	if err != nil {
		httpErr(w, err)
		return
	}
	if !soldOut {
		fail("Seats just opened up — grab one straight from the map!")
		return
	}
	owned, held, err := ticketUsage(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	if owned+held >= maxTicketsPerEvent {
		fail("You've already hit the 6-ticket limit for this event.")
		return
	}
	if _, err := db.Exec(`INSERT INTO waitlist (event_id, session_id, email) VALUES (?,?,?)`, eventID, sid, email); err != nil {
		httpErr(w, err)
		return
	}
	v, err := getWaitlistView(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	render(w, "waitlistcard.html", v)
}

func handleLeaveWaitlist(w http.ResponseWriter, r *http.Request) {
	eventID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sid := sessionID(r)

	tx, err := db.Begin()
	if err != nil {
		httpErr(w, err)
		return
	}
	defer tx.Rollback()
	var wID int64
	var status string
	var seatID sql.NullInt64
	err = tx.QueryRow(`SELECT id, status, offered_seat_id FROM waitlist
		WHERE event_id=? AND session_id=? AND status IN ('waiting','offered')
		ORDER BY id DESC LIMIT 1`, eventID, sid).Scan(&wID, &status, &seatID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		httpErr(w, err)
		return
	}
	if err == nil {
		if _, err := tx.Exec(`UPDATE waitlist SET status='cancelled' WHERE id=?`, wID); err != nil {
			httpErr(w, err)
			return
		}
		if status == "offered" && seatID.Valid {
			if err := releaseSeatOffers(tx, seatID.Int64, eventID); err != nil {
				httpErr(w, err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		httpErr(w, err)
		return
	}
	v, err := getWaitlistView(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	render(w, "waitlistcard.html", v)
}

// ---------- queries ----------

func eventSoldOut(eventID int64) (bool, error) {
	var n int64
	err := db.QueryRow(`SELECT COUNT(*) FROM seats WHERE event_id=? AND status='available'`, eventID).Scan(&n)
	return n == 0, err
}

// offerSeat gives a freed seat to the next eligible waitlist entry, or marks
// it available when nobody's waiting. Callers must hold a transaction.
func offerSeat(tx *sql.Tx, seatID, eventID int64) error {
	for {
		var wID int64
		var wsid string
		err := tx.QueryRow(`SELECT id, session_id FROM waitlist
			WHERE event_id=? AND status='waiting' ORDER BY id LIMIT 1`, eventID).Scan(&wID, &wsid)
		if errors.Is(err, sql.ErrNoRows) {
			_, err := tx.Exec(`UPDATE seats SET status='available', hold_session=NULL WHERE id=?`, seatID)
			return err
		}
		if err != nil {
			return err
		}
		// skip waiters who've hit the per-event cap
		owned, held, err := ticketUsage(eventID, wsid)
		if err != nil {
			return err
		}
		if owned+held >= maxTicketsPerEvent {
			if _, err := tx.Exec(`UPDATE waitlist SET status='expired' WHERE id=?`, wID); err != nil {
				return err
			}
			continue
		}
		expires := time.Now().Add(offerTTL).UTC().Format(time.RFC3339)
		if _, err := tx.Exec(`UPDATE waitlist SET status='offered', offered_seat_id=?,
			offered_at=strftime('%Y-%m-%dT%H:%M:%SZ','now'), offer_expires_at=? WHERE id=?`, seatID, expires, wID); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE seats SET status='held', hold_session=? WHERE id=?`, wsid, seatID)
		return err
	}
}

// releaseSeatOffers handles a seat its holder just gave up: any offer tied to
// it lapses, then the seat passes to the waitlist (or back to the pool).
func releaseSeatOffers(tx *sql.Tx, seatID, eventID int64) error {
	if _, err := tx.Exec(`UPDATE waitlist SET status='expired' WHERE offered_seat_id=? AND status='offered'`, seatID); err != nil {
		return err
	}
	return offerSeat(tx, seatID, eventID)
}

// sweepExpiredOffers re-offers seats whose claim window has lapsed. Runs
// lazily on page loads rather than in a background job.
func sweepExpiredOffers(eventID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, offered_seat_id FROM waitlist
		WHERE event_id=? AND status='offered' AND offer_expires_at < strftime('%Y-%m-%dT%H:%M:%SZ','now')`, eventID)
	if err != nil {
		return err
	}
	type expiredOffer struct{ wID, seatID int64 }
	var expired []expiredOffer
	for rows.Next() {
		var e expiredOffer
		if err := rows.Scan(&e.wID, &e.seatID); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, e)
	}
	rows.Close()
	for _, e := range expired {
		if _, err := tx.Exec(`UPDATE waitlist SET status='expired' WHERE id=?`, e.wID); err != nil {
			return err
		}
		if err := offerSeat(tx, e.seatID, eventID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// sweepForCart sweeps every event the session currently holds seats for.
func sweepForCart(sid string) error {
	seats, err := heldSeats(sid)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, s := range seats {
		if !seen[s.EventID] {
			seen[s.EventID] = true
			if err := sweepExpiredOffers(s.EventID); err != nil {
				return err
			}
		}
	}
	return nil
}

// getWaitlistView builds the sidebar card: join form when sold out, position
// while waiting, offer details when a seat is being held for this session.
// nil means render no card.
func getWaitlistView(eventID int64, sid string) (*WaitlistView, error) {
	var wID int64
	var status, email string
	var seatID sql.NullInt64
	var expires sql.NullString
	err := db.QueryRow(`SELECT id, status, email, offered_seat_id, offer_expires_at FROM waitlist
		WHERE event_id=? AND session_id=? AND status IN ('waiting','offered')
		ORDER BY id DESC LIMIT 1`, eventID, sid).Scan(&wID, &status, &email, &seatID, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		soldOut, err := eventSoldOut(eventID)
		if err != nil || !soldOut {
			return nil, err
		}
		return &WaitlistView{EventID: eventID, State: "join"}, nil
	case err != nil:
		return nil, err
	}
	v := &WaitlistView{EventID: eventID, Email: email}
	switch status {
	case "waiting":
		v.State = "waiting"
		if err := db.QueryRow(`SELECT COUNT(*)+1 FROM waitlist WHERE event_id=? AND status='waiting' AND id<?`, eventID, wID).Scan(&v.Position); err != nil {
			return nil, err
		}
	case "offered":
		v.State = "offered"
		if err := db.QueryRow(`SELECT section||' · '||row||number FROM seats WHERE id=?`, seatID.Int64).Scan(&v.SeatLabel); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, expires.String); err == nil {
			v.MinutesLeft = int64(math.Ceil(time.Until(t).Minutes()))
		}
	}
	return v, nil
}

func getEvent(id int64) (Event, error) {
	var e Event
	err := db.QueryRow(`SELECT id,artist,title,venue,city,starts_at,description,gradient FROM events WHERE id=?`, id).
		Scan(&e.ID, &e.Artist, &e.Title, &e.Venue, &e.City, &e.StartsAt, &e.Description, &e.Gradient)
	return e, err
}

func getSections(eventID int64, sid string) ([]SeatSection, error) {
	rows, err := db.Query(`SELECT id,event_id,section,row,number,price_cents,status,COALESCE(hold_session,'')
		FROM seats WHERE event_id=? ORDER BY section, row, number`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sections []SeatSection
	secIdx := map[string]int{}
	rowIdx := map[string]int{} // "section|row" -> index in section.Rows
	for rows.Next() {
		var s Seat
		if err := rows.Scan(&s.ID, &s.EventID, &s.Section, &s.Row, &s.Number, &s.PriceCents, &s.Status, &s.HoldSession); err != nil {
			return nil, err
		}
		s.Viewer = sid
		si, ok := secIdx[s.Section]
		if !ok {
			sections = append(sections, SeatSection{Name: s.Section, PriceCents: s.PriceCents})
			si = len(sections) - 1
			secIdx[s.Section] = si
		}
		key := s.Section + "|" + s.Row
		ri, ok := rowIdx[key]
		if !ok {
			sections[si].Rows = append(sections[si].Rows, SeatRow{Label: s.Row})
			ri = len(sections[si].Rows) - 1
			rowIdx[key] = ri
		}
		sections[si].Rows[ri].Seats = append(sections[si].Rows[ri].Seats, s)
	}
	return sections, rows.Err()
}

func getCart(eventID int64, sid string) (CartView, error) {
	rows, err := db.Query(`SELECT id,event_id,section,row,number,price_cents,status,COALESCE(hold_session,'')
		FROM seats WHERE event_id=? AND status='held' AND hold_session=? ORDER BY section,row,number`, eventID, sid)
	if err != nil {
		return CartView{}, err
	}
	defer rows.Close()
	cart := CartView{EventID: eventID, Max: maxTicketsPerEvent}
	for rows.Next() {
		var s Seat
		if err := rows.Scan(&s.ID, &s.EventID, &s.Section, &s.Row, &s.Number, &s.PriceCents, &s.Status, &s.HoldSession); err != nil {
			return CartView{}, err
		}
		cart.Seats = append(cart.Seats, s)
		cart.TotalCents += s.PriceCents
	}
	if rows.Err() != nil {
		return CartView{}, rows.Err()
	}
	owned, _, err := ticketUsage(eventID, sid)
	if err != nil {
		return CartView{}, err
	}
	cart.Used = owned + int64(len(cart.Seats))
	return cart, nil
}

// promoByCode looks up a promo by its (already-uppercased) code.
func promoByCode(code string) (Promo, error) {
	var p Promo
	var exp sql.NullString
	var max sql.NullInt64
	err := db.QueryRow(`SELECT id,code,percent_off,expires_at,max_uses,uses FROM promos WHERE code=?`, code).
		Scan(&p.ID, &p.Code, &p.PercentOff, &exp, &max, &p.Uses)
	if err != nil {
		return p, err
	}
	if exp.Valid && exp.String != "" {
		p.ExpiresAt, _ = time.Parse(time.RFC3339, exp.String)
	}
	if max.Valid {
		p.MaxUses = max.Int64
	}
	return p, nil
}

// promoForSession returns the banner promo to show this session, or nil.
// Eligibility is constant for now — once banners should only target
// brand-new buyers, gate on this session's order history here.
func promoForSession(sid string) *Promo {
	rows, err := db.Query(`SELECT id,code,percent_off,expires_at,max_uses,uses FROM promos ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var p Promo
		var exp sql.NullString
		var max sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Code, &p.PercentOff, &exp, &max, &p.Uses); err != nil {
			return nil
		}
		if exp.Valid && exp.String != "" {
			p.ExpiresAt, _ = time.Parse(time.RFC3339, exp.String)
		}
		if max.Valid {
			p.MaxUses = max.Int64
		}
		if p.Validate() == "" {
			return &p
		}
	}
	return nil
}

// ticketUsage counts tickets the session already bought plus seats it holds,
// for one event. The per-event cap is enforced against the sum.
func ticketUsage(eventID int64, sid string) (owned, held int64, err error) {
	if err = db.QueryRow(`SELECT COUNT(*) FROM tickets t
		JOIN orders o ON o.id = t.order_id
		JOIN seats s ON s.id = t.seat_id
		WHERE o.session_id = ? AND s.event_id = ?`, sid, eventID).Scan(&owned); err != nil {
		return
	}
	err = db.QueryRow(`SELECT COUNT(*) FROM seats
		WHERE event_id = ? AND status = 'held' AND hold_session = ?`, eventID, sid).Scan(&held)
	return
}

// heldSeats returns all seats this session holds, across events.
func heldSeats(sid string) ([]Seat, error) {
	rows, err := db.Query(`SELECT s.id,s.event_id,s.section,s.row,s.number,s.price_cents,s.status,COALESCE(s.hold_session,''),
		       e.artist || ' — ' || e.title
		FROM seats s JOIN events e ON e.id = s.event_id
		WHERE s.status='held' AND s.hold_session=? ORDER BY s.event_id,s.section,s.row,s.number`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seat
	for rows.Next() {
		var s Seat
		if err := rows.Scan(&s.ID, &s.EventID, &s.Section, &s.Row, &s.Number, &s.PriceCents, &s.Status, &s.HoldSession, &s.EventName); err != nil {
			return nil, err
		}
		s.Viewer = sid
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------- rendering ----------

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}

// qrSVG renders a deterministic QR-style matrix from the ticket code.
// It is decorative — a real deployment would use an actual QR encoder.
func qrSVG(code string) template.HTML {
	h := sha256.Sum256([]byte(code))
	const n = 21
	var b strings.Builder
	b.WriteString(`<svg viewBox="0 0 21 21" class="w-full h-full" shape-rendering="crispEdges">`)
	b.WriteString(`<rect width="21" height="21" fill="#fff"/>`)
	cell := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			// finder patterns in three corners
			inFinder := (x < 7 && y < 7) || (x >= n-7 && y < 7) || (x < 7 && y >= n-7)
			if inFinder {
				lx, ly := x%7, y%7
				ring := lx == 0 || lx == 6 || ly == 0 || ly == 6 || (lx >= 2 && lx <= 4 && ly >= 2 && ly <= 4)
				if ring {
					fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1" fill="#000"/>`, x, y)
				}
				continue
			}
			byteIdx := (cell / 8) % len(h)
			bit := (h[byteIdx] >> (cell % 8)) & 1
			if bit == 1 {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1" fill="#000"/>`, x, y)
			}
			cell++
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func httpErr(w http.ResponseWriter, err error) {
	http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
}
