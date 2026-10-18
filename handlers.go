package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
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
	Tier        string // Premium | Standard | Value
	HoldSession string
	ResaleCents int64  // >0 when a listed resale ticket lives on this seat
	Viewer      string // requesting session, used to render state
	EventName   string // "Artist — Title", populated for cart/checkout views
}

// Mine reports whether the current session holds this seat.
func (s Seat) Mine() bool {
	return s.Status == "held" && s.HoldSession == s.Viewer
}

// Label is a short human label like "B12".
func (s Seat) Label() string { return s.Row + strconv.FormatInt(s.Number, 10) }

// CartCents is what the buyer pays for this seat: the resale price when it is
// a resale listing, else the face price.
func (s Seat) CartCents() int64 {
	if s.ResaleCents > 0 {
		return s.ResaleCents
	}
	return s.PriceCents
}

// SeatState resolves how to render the seat for the requesting session.
func (s Seat) SeatState() string {
	switch {
	case s.Mine():
		return "selected"
	case s.Status == "held":
		return "sold" // held by someone else looks taken
	case s.Status == "sold" && s.ResaleCents > 0:
		return "resale"
	case s.Status == "sold":
		return "sold"
	default:
		return "available"
	}
}

type SeatRow struct {
	Label string
	Seats []Seat
}

type SeatTier struct {
	Name       string
	PriceCents int64
}

type SeatSection struct {
	Name       string
	PriceCents int64
	Tiers      []SeatTier // distinct tiers in front-to-back order
	Rows       []SeatRow
}

const maxTicketsPerEvent = 6

// groupDealPct is the automatic discount applied to every contiguous run of
// 2+ seats in the same row — "sit together, save together".
const groupDealPct = 10

// groupDiscount sums the deal across a set of seats. Seats are grouped by
// event+section+row; each maximal run of consecutive seat numbers of length
// 2+ earns groupDealPct off its subtotal.
func groupDiscount(seats []Seat) int64 {
	type key struct {
		eventID      int64
		section, row string
	}
	runs := map[key][]Seat{}
	var order []key
	for _, s := range seats {
		k := key{s.EventID, s.Section, s.Row}
		if _, ok := runs[k]; !ok {
			order = append(order, k)
		}
		runs[k] = append(runs[k], s)
	}
	var total int64
	for _, k := range order {
		g := runs[k]
		sort.Slice(g, func(i, j int) bool { return g[i].Number < g[j].Number })
		start := 0
		for i := 1; i <= len(g); i++ {
			if i < len(g) && g[i].Number == g[i-1].Number+1 {
				continue
			}
			if i-start >= 2 {
				var run int64
				for _, s := range g[start:i] {
					run += s.PriceCents
				}
				total += run * groupDealPct / 100
			}
			start = i
		}
	}
	return total
}

func countByEvent(seats []Seat) map[int64]int {
	m := map[int64]int{}
	for _, s := range seats {
		m[s.EventID]++
	}
	return m
}

type CartView struct {
	EventID    int64
	Seats      []Seat
	TotalCents int64 // net of group deal
	DealCents  int64 // automatic adjacent-seat discount
	Used       int64 // seats already owned + currently held for this event
	Max        int64
	HasResale  bool
	Err        string
}

type eventPage struct {
	Event    Event
	Sections []SeatSection
	Cart     CartView
	Promo    *Promo
	Waitlist *WaitlistView
	Resale   []Seat
}

// offerTTL is how long a waitlist offer holds a seat. OFFER_TTL_SECONDS
// overrides the default at startup; unset or non-positive means 900.
var offerTTL = 15 * time.Minute

// WaitlistView is the sidebar card for a sold-out event.
// State is "join", "waiting" or "offered"; empty means show nothing.
type WaitlistView struct {
	EventID     int64
	State       string
	Position    int64
	Email       string
	SeatLabel   string
	ResaleCents int64
	MinutesLeft int64
	Err         string
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
	ID                 int64
	SessionID          string
	Name               string
	Email              string
	TotalCents         int64
	DiscountCents      int64
	GroupDiscountCents int64
	PromoCode          string
	Status             string
	RefundedAt         string
	CreatedAt          string
}

type Ticket struct {
	ID               int64
	Code             string
	Section          string
	Row              string
	Number           int64
	Tier             string
	CheckedInAt      string
	HolderName       string
	HolderEmail      string
	ListingCents     int64
	Resold           bool
	ResalePriceCents int64
	PayoutCents      int64
	Kind             string
	Transfers        int64
	Transferred      bool
	TransferredTo    string
	CanTransfer      bool
	EventID          int64
	Artist           string
	Title            string
	Venue            string
	City             string
	StartsAt         string
	Gradient         string
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
	var resale []Seat
	for _, sec := range sections {
		for _, r := range sec.Rows {
			for _, seat := range r.Seats {
				if seat.Status == "sold" && seat.ResaleCents > 0 {
					resale = append(resale, seat)
				}
			}
		}
	}
	render(w, "event.html", eventPage{Event: e, Sections: sections, Cart: cart, Promo: promoForSession(sid), Waitlist: wl, Resale: resale})
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
	err = tx.QueryRow(`SELECT s.id,s.event_id,s.section,s.row,s.number,s.price_cents,s.status,COALESCE(s.hold_session,''),s.tier,
		COALESCE((SELECT t2.listing_price_cents FROM tickets t2 JOIN orders o2 ON o2.id=t2.order_id WHERE t2.seat_id=s.id AND t2.resold=0 AND o2.status='paid' AND t2.listing_price_cents>0),0)
		FROM seats s WHERE s.id=? AND s.event_id=?`, seatID, eventID).
		Scan(&seat.ID, &seat.EventID, &seat.Section, &seat.Row, &seat.Number, &seat.PriceCents, &seat.Status, &seat.HoldSession, &seat.Tier, &seat.ResaleCents)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	seat.Viewer = sid
	var capErr string
	switch {
	case seat.Status == "sold" && seat.ResaleCents > 0:
		// a listed resale seat — hold it like any seat, but never to its own seller
		var sellerSid string
		if err := tx.QueryRow(`SELECT o2.session_id FROM tickets t2 JOIN orders o2 ON o2.id=t2.order_id WHERE t2.seat_id=? AND t2.resold=0 AND o2.status='paid' AND t2.listing_price_cents>0`, seat.ID).Scan(&sellerSid); err != nil {
			httpErr(w, err)
			return
		}
		if sellerSid == sid {
			capErr = "You cannot buy your own resale listing."
			break
		}
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
		if seat.ResaleCents > 0 {
			// back to a live listing, not to the pool
			if _, err := tx.Exec(`UPDATE seats SET status='sold', hold_session=NULL WHERE id=?`, seat.ID); err != nil {
				httpErr(w, err)
				return
			}
		} else {
			if _, err := tx.Exec(`UPDATE seats SET status='available', hold_session=NULL WHERE id=?`, seat.ID); err != nil {
				httpErr(w, err)
				return
			}
			// a released seat goes to the next person in line, if anyone's waiting
			if err := releaseSeatOffers(tx, seat.ID, eventID); err != nil {
				httpErr(w, err)
				return
			}
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

// findBestBlock returns the IDs of the best block of n contiguous seats:
// nearest the stage first (section stage_rank, then frontmost row), then the
// window closest to the row's center. Returns nil when no row can fit n.
func findBestBlock(eventID int64, n int) ([]int64, error) {
	rows, err := db.Query(`SELECT id, section, row, number, status FROM seats
		WHERE event_id=? ORDER BY stage_rank, row, number`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rowSeat struct {
		id     int64
		number int64
		avail  bool
	}
	type rowGroup struct {
		section, row string
		seats        []rowSeat
	}
	var groups []rowGroup
	for rows.Next() {
		var s rowSeat
		var sec, row, status string
		if err := rows.Scan(&s.id, &sec, &row, &s.number, &status); err != nil {
			return nil, err
		}
		s.avail = status == "available"
		if len(groups) == 0 || groups[len(groups)-1].section != sec || groups[len(groups)-1].row != row {
			groups = append(groups, rowGroup{section: sec, row: row})
		}
		g := &groups[len(groups)-1]
		g.seats = append(g.seats, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, g := range groups {
		center := float64(g.seats[0].number+g.seats[len(g.seats)-1].number) / 2
		bestDist, bestStart := math.Inf(1), -1
		for i := 0; i+n <= len(g.seats); i++ {
			win := g.seats[i : i+n]
			ok := true
			for j, s := range win {
				if !s.avail || (j > 0 && s.number != win[j-1].number+1) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			mid := float64(win[0].number+win[n-1].number) / 2
			if d := math.Abs(mid - center); d < bestDist {
				bestDist, bestStart = d, i
			}
		}
		if bestStart >= 0 {
			ids := make([]int64, n)
			for j, s := range g.seats[bestStart : bestStart+n] {
				ids[j] = s.id
			}
			return ids, nil
		}
	}
	return nil, nil
}

// handlePickBest holds the best available block of n seats for the session.
func handlePickBest(w http.ResponseWriter, r *http.Request) {
	eventID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sid := sessionID(r)
	n, _ := strconv.Atoi(r.FormValue("n"))

	fail := func(msg string) {
		sections, err1 := getSections(eventID, sid)
		cart, err2 := getCart(eventID, sid)
		if err1 != nil || err2 != nil {
			httpErr(w, errors.Join(err1, err2))
			return
		}
		cart.Err = msg
		render(w, "clearresp.html", struct {
			Sections []SeatSection
			Cart     CartView
		}{sections, cart})
	}

	if err := sweepExpiredOffers(eventID); err != nil {
		httpErr(w, err)
		return
	}
	if n < 1 {
		fail("Pick at least 1 seat.")
		return
	}
	owned, held, err := ticketUsage(eventID, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	if left := maxTicketsPerEvent - owned - held; int64(n) > left {
		fail(fmt.Sprintf("Only %d of your %d tickets left — release some seats or pick fewer.", left, maxTicketsPerEvent))
		return
	}
	ids, err := findBestBlock(eventID, n)
	if err != nil {
		httpErr(w, err)
		return
	}
	if ids == nil {
		fail(fmt.Sprintf("No block of %d seats together left — try fewer or grab singles on the map.", n))
		return
	}

	tx, err := db.Begin()
	if err != nil {
		httpErr(w, err)
		return
	}
	defer tx.Rollback()
	var lost bool
	for _, id := range ids {
		res, err := tx.Exec(`UPDATE seats SET status='held', hold_session=? WHERE id=? AND status='available'`, sid, id)
		if err != nil {
			httpErr(w, err)
			return
		}
		if aff, _ := res.RowsAffected(); aff == 0 {
			lost = true // someone grabbed it mid-search
		}
	}
	if lost {
		tx.Rollback()
		fail("Those seats just got snapped up — try again!")
		return
	}
	if err := tx.Commit(); err != nil {
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
	render(w, "clearresp.html", struct {
		Sections []SeatSection
		Cart     CartView
	}{sections, cart})
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
		var listed int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tickets t2 JOIN orders o2 ON o2.id=t2.order_id WHERE t2.seat_id=? AND t2.resold=0 AND o2.status='paid' AND t2.listing_price_cents>0`, seatID).Scan(&listed); err != nil {
			httpErr(w, err)
			return
		}
		if listed > 0 {
			if _, err := tx.Exec(`UPDATE seats SET status='sold', hold_session=NULL WHERE id=?`, seatID); err != nil {
				httpErr(w, err)
				return
			}
			continue
		}
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
	GroupCents    int64 // automatic adjacent-seat deal
	DiscountCents int64 // promo code discount
	TotalCents    int64
	PromoCode     string
	PromoErr      string
	HasResale     bool
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
	var normal []Seat
	for _, s := range seats {
		v.SubtotalCents += s.CartCents()
		if s.ResaleCents == 0 {
			normal = append(normal, s)
		} else {
			v.HasResale = true
		}
	}
	var normalSub int64
	for _, s := range normal {
		normalSub += s.PriceCents
	}
	v.GroupCents = groupDiscount(normal)
	// promos stack on the deal price of normal seats only — resale seats are
	// never discounted and never join a run
	base := normalSub - v.GroupCents
	if promoCode != "" && base > 0 {
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
				v.DiscountCents = base * p.PercentOff / 100
			}
		}
	}
	v.TotalCents = base - v.DiscountCents + (v.SubtotalCents - normalSub)
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
	rows, err := tx.Query(`SELECT s.id, s.event_id, s.section, s.row, s.number, s.price_cents,
		COALESCE((SELECT t2.listing_price_cents FROM tickets t2 JOIN orders o2 ON o2.id=t2.order_id WHERE t2.seat_id=s.id AND t2.resold=0 AND o2.status='paid' AND t2.listing_price_cents>0),0)
		FROM seats s WHERE s.status='held' AND s.hold_session=?`, sid)
	if err != nil {
		httpErr(w, err)
		return
	}
	var held []Seat
	var subtotal int64
	for rows.Next() {
		var s Seat
		if err := rows.Scan(&s.ID, &s.EventID, &s.Section, &s.Row, &s.Number, &s.PriceCents, &s.ResaleCents); err != nil {
			rows.Close()
			httpErr(w, err)
			return
		}
		held = append(held, s)
		subtotal += s.CartCents()
	}
	rows.Close()
	if len(held) == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// re-verify the per-event cap against owned tickets + this cart, per event
	for ev, n := range countByEvent(held) {
		var owned int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tickets t
			JOIN orders o ON o.id = t.order_id
			JOIN seats s ON s.id = t.seat_id
			WHERE o.session_id = ? AND s.event_id = ?`, sid, ev).Scan(&owned); err != nil {
			httpErr(w, err)
			return
		}
		if owned+int64(n) > maxTicketsPerEvent {
			fail(fmt.Sprintf("Limit of %d tickets per event — you already have %d.", maxTicketsPerEvent, owned))
			return
		}
	}

	var normalHeld []Seat
	var normalSub int64
	for _, hs := range held {
		if hs.ResaleCents == 0 {
			normalHeld = append(normalHeld, hs)
			normalSub += hs.PriceCents
		}
	}
	group := groupDiscount(normalHeld)
	base := normalSub - group // promos stack on the deal price, same as display

	var discount int64
	var appliedCode string
	if promoInput != "" && base > 0 {
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
		discount = base * p.PercentOff / 100
		appliedCode = p.Code
	}
	total := base - discount

	total = base - discount + (subtotal - normalSub) // resale seats full price on top
	res, err := tx.Exec(`INSERT INTO orders (session_id,name,email,total_cents,promo_code,discount_cents,group_discount_cents) VALUES (?,?,?,?,?,?,?)`,
		sid, name, email, total, appliedCode, discount, group)
	if err != nil {
		httpErr(w, err)
		return
	}
	orderID, _ := res.LastInsertId()

	for _, hs := range held {
		if _, err := tx.Exec(`UPDATE seats SET status='sold', hold_session=NULL WHERE id=? AND status='held' AND hold_session=?`, hs.ID, sid); err != nil {
			httpErr(w, err)
			return
		}
		if _, err := tx.Exec(`UPDATE waitlist SET status='claimed' WHERE offered_seat_id=? AND status='offered' AND session_id=?`, hs.ID, sid); err != nil {
			httpErr(w, err)
			return
		}
		if hs.ResaleCents > 0 {
			// resale: the seller's ticket dies, a fresh ticket goes to the buyer
			var oldTID, oldOID, face int64
			err := tx.QueryRow(`SELECT t2.id, t2.order_id, t2.price_cents FROM tickets t2 JOIN orders o2 ON o2.id=t2.order_id
				WHERE t2.seat_id=? AND t2.resold=0 AND o2.status='paid' AND t2.listing_price_cents>0`, hs.ID).
				Scan(&oldTID, &oldOID, &face)
			if err != nil {
				httpErr(w, err)
				return
			}
			fee := (hs.ResaleCents*5 + 50) / 100 // 5%, halves round up
			if _, err := tx.Exec(`UPDATE tickets SET resold=1, resale_price_cents=?, listing_price_cents=0 WHERE id=?`, hs.ResaleCents, oldTID); err != nil {
				httpErr(w, err)
				return
			}
			if _, err := tx.Exec(`INSERT INTO resales (ticket_id,event_id,seller_order_id,buyer_order_id,price_cents,fee_cents,payout_cents,created_at)
				VALUES (?,?,?,?,?,?,?,strftime('%Y-%m-%dT%H:%M:%SZ','now'))`, oldTID, hs.EventID, oldOID, orderID, hs.ResaleCents, fee, hs.ResaleCents-fee); err != nil {
				httpErr(w, err)
				return
			}
			if _, err := tx.Exec(`INSERT INTO tickets (order_id, seat_id, code, price_cents, kind, holder_name, holder_email) VALUES (?,?,?,?,?,?,?)`,
				orderID, hs.ID, "TP-"+strings.ToUpper(newID(5)), face, "resale", name, email); err != nil {
				httpErr(w, err)
				return
			}
			continue
		}
		if _, err := tx.Exec(`INSERT INTO tickets (order_id, seat_id, code, price_cents) VALUES (?,?,?,?)`, orderID, hs.ID, "TP-"+strings.ToUpper(newID(5)), hs.PriceCents); err != nil {
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
	err = db.QueryRow(`SELECT id,session_id,name,email,total_cents,discount_cents,group_discount_cents,promo_code,status,refunded_at,created_at FROM orders WHERE id=?`, orderID).
		Scan(&o.ID, &o.SessionID, &o.Name, &o.Email, &o.TotalCents, &o.DiscountCents, &o.GroupDiscountCents, &o.PromoCode, &o.Status, &refundedAt, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	rows, err := db.Query(`
		SELECT t.id, t.code, s.section, s.row, s.number, s.tier, COALESCE(t.checked_in_at,''),
		       t.holder_name, t.holder_email, t.listing_price_cents, t.resold, t.resale_price_cents, t.kind,
		       (SELECT COUNT(*) FROM transfers tr WHERE tr.ticket_id=t.id),
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
	refundable := true
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.ID, &t.Code, &t.Section, &t.Row, &t.Number, &t.Tier, &t.CheckedInAt,
			&t.HolderName, &t.HolderEmail, &t.ListingCents, &t.Resold, &t.ResalePriceCents, &t.Kind, &t.Transfers,
			&t.EventID, &t.Artist, &t.Title, &t.Venue, &t.City, &t.StartsAt, &t.Gradient); err != nil {
			httpErr(w, err)
			return
		}
		t.Transferred = t.Transfers > 0
		t.TransferredTo = t.HolderName
		t.CanTransfer = o.Status == "paid" && t.CheckedInAt == "" && !t.Transferred && !t.Resold && t.ListingCents == 0 && t.Transfers < 3
		t.PayoutCents = t.ResalePriceCents - (t.ResalePriceCents*5+50)/100
		if t.Transferred || t.Resold || t.ListingCents > 0 || t.Kind == "resale" {
			refundable = false
		}
		tickets = append(tickets, t)
	}
	o.RefundedAt = refundedAt.String
	render(w, "order.html", struct {
		Order      Order
		Tickets    []Ticket
		IsOwner    bool
		Refundable bool
	}{o, tickets, o.SessionID == sessionID(r), refundable})
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

	var moved, listed, boughtResale int64
	if err := tx.QueryRow(`SELECT
		(SELECT COUNT(*) FROM tickets t WHERE t.order_id=? AND (t.resold=1 OR EXISTS(SELECT 1 FROM transfers tr WHERE tr.ticket_id=t.id))),
		(SELECT COUNT(*) FROM tickets t WHERE t.order_id=? AND t.listing_price_cents>0 AND t.resold=0),
		(SELECT COUNT(*) FROM tickets t WHERE t.order_id=? AND t.kind='resale')`, orderID, orderID, orderID).
		Scan(&moved, &listed, &boughtResale); err != nil {
		httpErr(w, err)
		return
	}
	switch {
	case moved > 0:
		http.Error(w, "This order cannot be refunded because a ticket in it was transferred or resold.", http.StatusConflict)
		return
	case listed > 0:
		http.Error(w, "This order has a ticket listed for resale. Delist it before refunding.", http.StatusConflict)
		return
	case boughtResale > 0:
		http.Error(w, "Resale purchases are final and cannot be refunded.", http.StatusConflict)
		return
	}

	if _, err := tx.Exec(`UPDATE orders SET status='refunded', refunded_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`, orderID); err != nil {
		httpErr(w, err)
		return
	}

	// a dead ticket shouldn't carry door state — its seat may be resold
	if _, err := tx.Exec(`UPDATE tickets SET checked_in_at=NULL, checked_in_by='' WHERE order_id=?`, orderID); err != nil {
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

// ---------- door check-in ----------
//
// Each event has a door code (events.checkin_code) the host can hand to staff.
// The code is the whole credential: it opens /checkin/{code} and authorizes
// scans for that event only. Rotating it kills the old link instantly.
// Door devices are shared and sessionless — check-in deliberately ignores the
// sid cookie.

// normalizeTicketCode tolerates how door staff actually type: lowercase,
// spaces, and a missing "TP-" prefix all resolve to the canonical code.
func normalizeTicketCode(s string) string {
	s = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	s = strings.TrimPrefix(s, "TP-")
	if s == "" {
		return ""
	}
	return "TP-" + s
}

func eventByCheckinCode(code string) (Event, error) {
	var e Event
	err := db.QueryRow(`SELECT id,artist,title,venue,city,starts_at,description,gradient FROM events WHERE checkin_code=?`, code).
		Scan(&e.ID, &e.Artist, &e.Title, &e.Venue, &e.City, &e.StartsAt, &e.Description, &e.Gradient)
	return e, err
}

// CheckinResult is one scan outcome, rendered as a card or JSON.
type CheckinResult struct {
	OK          bool
	Result      string // JSON result override (transferred, resold); empty = derive from status
	Msg         string
	Code        string
	Section     string
	Row         string
	Number      int64
	Holder      string
	CheckedInAt string
}

func (r CheckinResult) SeatLabel() string {
	return r.Section + " · " + r.Row + strconv.FormatInt(r.Number, 10)
}

// checkinRespond sends the result card to htmx callers and JSON to anything
// asking for it — a real scanner can integrate against the same endpoint.
// Status codes are real in both modes: 200 first scan, 409 rescan, 410 void,
// 422 wrong event, 404 unknown ticket/door code, 400 empty field.
func checkinRespond(w http.ResponseWriter, r *http.Request, status int, res CheckinResult) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		result := res.Result
		if result == "" {
			result = "checked_in"
		}
		if !res.OK && res.Result == "" {
			switch status {
			case http.StatusConflict:
				result = "already_checked_in"
			case http.StatusGone:
				result = "void"
			case http.StatusUnprocessableEntity:
				result = "wrong_event"
			case http.StatusBadRequest:
				result = "invalid"
			default:
				result = "not_found"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{
			"result":        result,
			"code":          res.Code,
			"seat":          res.SeatLabel(),
			"holder":        res.Holder,
			"checked_in_at": res.CheckedInAt,
			"message":       res.Msg,
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tpl.ExecuteTemplate(w, "checkinresult.html", res); err != nil {
		log.Printf("template checkinresult.html: %v", err)
	}
}

func checkinResultFor(code string) (CheckinResult, error) {
	var res CheckinResult
	err := db.QueryRow(`SELECT t.code, COALESCE(t.checked_in_at,''), s.section, s.row, s.number, COALESCE(NULLIF(t.holder_name,''),o.name)
		FROM tickets t JOIN seats s ON s.id=t.seat_id JOIN orders o ON o.id=t.order_id
		WHERE t.code=?`, code).
		Scan(&res.Code, &res.CheckedInAt, &res.Section, &res.Row, &res.Number, &res.Holder)
	return res, err
}

type checkinScan struct {
	Code        string
	Section     string
	Label       string
	Holder      string
	CheckedInAt string
}

type checkinStatus struct {
	CheckedIn int64
	Sold      int64
	Recent    []checkinScan
}

// getCheckinStatus powers the door page's live counter: paid tickets scanned
// vs paid tickets sold, plus the latest scans from every staff device.
func getCheckinStatus(eventID int64) (checkinStatus, error) {
	var st checkinStatus
	err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM tickets t JOIN orders o ON o.id=t.order_id JOIN seats s ON s.id=t.seat_id
		  WHERE s.event_id=? AND o.status='paid' AND t.resold=0 AND t.checked_in_at IS NOT NULL),
		(SELECT COUNT(*) FROM tickets t JOIN orders o ON o.id=t.order_id JOIN seats s ON s.id=t.seat_id
		  WHERE s.event_id=? AND o.status='paid' AND t.resold=0)`, eventID, eventID).
		Scan(&st.CheckedIn, &st.Sold)
	if err != nil {
		return st, err
	}
	rows, err := db.Query(`SELECT t.code, s.section, s.row||s.number, COALESCE(NULLIF(t.holder_name,''),o.name), t.checked_in_at
		FROM tickets t JOIN orders o ON o.id=t.order_id JOIN seats s ON s.id=t.seat_id
		WHERE s.event_id=? AND t.checked_in_at IS NOT NULL
		ORDER BY t.checked_in_at DESC, t.id DESC LIMIT 8`, eventID)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var sc checkinScan
		if err := rows.Scan(&sc.Code, &sc.Section, &sc.Label, &sc.Holder, &sc.CheckedInAt); err != nil {
			return st, err
		}
		st.Recent = append(st.Recent, sc)
	}
	return st, rows.Err()
}

func handleCheckinPage(w http.ResponseWriter, r *http.Request) {
	door := strings.ToUpper(strings.TrimSpace(r.PathValue("doorCode")))
	e, err := eventByCheckinCode(door)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "This check-in link is invalid or was rotated. Ask the host for a fresh one.", http.StatusNotFound)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	st, err := getCheckinStatus(e.ID)
	if err != nil {
		httpErr(w, err)
		return
	}
	// the door code lives in the URL — don't leak it to CDN/font requests
	w.Header().Set("Referrer-Policy", "no-referrer")
	render(w, "checkin.html", struct {
		Event  Event
		Door   string
		Status checkinStatus
	}{e, door, st})
}

// handleCheckinStatus is the door page's 5-second poll target — a fragment so
// the live counter can refresh without stealing focus from the code input.
func handleCheckinStatus(w http.ResponseWriter, r *http.Request) {
	door := strings.ToUpper(strings.TrimSpace(r.PathValue("doorCode")))
	e, err := eventByCheckinCode(door)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "door code rotated", http.StatusNotFound)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	st, err := getCheckinStatus(e.ID)
	if err != nil {
		httpErr(w, err)
		return
	}
	render(w, "checkinstatus.html", st)
}

// handleCheckinScan marks a ticket checked in. A single guarded UPDATE makes
// the first-scan-wins rule atomic — SQLite serializes writers, so a racing
// second scan affects 0 rows and classifies as 409.
func handleCheckinScan(w http.ResponseWriter, r *http.Request) {
	door := strings.ToUpper(strings.TrimSpace(r.PathValue("doorCode")))
	e, err := eventByCheckinCode(door)
	if errors.Is(err, sql.ErrNoRows) {
		checkinRespond(w, r, http.StatusNotFound, CheckinResult{Msg: "This check-in link is invalid or was rotated."})
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	code := normalizeTicketCode(r.FormValue("ticket"))
	if code == "" {
		checkinRespond(w, r, http.StatusBadRequest, CheckinResult{Msg: "Enter a ticket code first."})
		return
	}
	res, err := db.Exec(`UPDATE tickets SET checked_in_at=strftime('%Y-%m-%dT%H:%M:%SZ','now'), checked_in_by=?, listing_price_cents=0
		WHERE code=? AND checked_in_at IS NULL AND resold=0
		AND seat_id IN (SELECT id FROM seats WHERE event_id=?)
		AND order_id IN (SELECT id FROM orders WHERE status='paid')`, door, code, e.ID)
	if err != nil {
		httpErr(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		// a door scan ends a listing — and any buyer's hold on that seat
		db.Exec(`UPDATE seats SET status='sold', hold_session=NULL
			WHERE status='held' AND id=(SELECT seat_id FROM tickets WHERE code=?)`, code)
		db.Exec(`UPDATE waitlist SET status='expired' WHERE status='offered'
			AND offered_seat_id=(SELECT seat_id FROM tickets WHERE code=?)`, code)
		ok, err := checkinResultFor(code)
		if err != nil {
			httpErr(w, err)
			return
		}
		ok.OK = true
		checkinRespond(w, r, http.StatusOK, ok)
		return
	}
	// Status codes are real in both modes: 200 first scan, 409 rescan, 410 void,
	// 422 wrong event, 404 unknown ticket/door code, 400 empty field.
	// 0 rows — classify in a fixed order so overlapping states (e.g. a ticket
	// checked in then refunded) get one deterministic answer
	var evID, number int64
	var status, checkedAt, section, row, holder string
	var resold int64
	err = db.QueryRow(`SELECT s.event_id, o.status, COALESCE(t.checked_in_at,''), s.section, s.row, s.number,
		COALESCE(NULLIF(t.holder_name,''),o.name), t.resold
		FROM tickets t JOIN seats s ON s.id=t.seat_id JOIN orders o ON o.id=t.order_id
		WHERE t.code=?`, code).
		Scan(&evID, &status, &checkedAt, &section, &row, &number, &holder, &resold)
	if errors.Is(err, sql.ErrNoRows) {
		var h string
		herr := db.QueryRow(`SELECT holder_name FROM transfers WHERE old_code=?`, code).Scan(&h)
		if herr == nil {
			checkinRespond(w, r, http.StatusGone, CheckinResult{Code: code, Result: "transferred", Holder: h,
				Msg: "That ticket was transferred to " + h + ". It is no longer valid."})
			return
		}
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		checkinRespond(w, r, http.StatusNotFound, CheckinResult{Code: code, Msg: "Unknown ticket code."})
	case err != nil:
		httpErr(w, err)
	case resold == 1:
		checkinRespond(w, r, http.StatusGone, CheckinResult{Code: code, Result: "resold", Holder: holder,
			Msg: "That ticket was sold on resale. It is no longer valid."})
	case evID != e.ID:
		checkinRespond(w, r, http.StatusUnprocessableEntity, CheckinResult{Code: code, Msg: "That ticket is for a different event."})
	case status != "paid":
		checkinRespond(w, r, http.StatusGone, CheckinResult{Code: code, Msg: "That ticket was refunded — it's no longer valid."})
	default:
		msg := "Already checked in"
		if t, perr := time.Parse(time.RFC3339, checkedAt); perr == nil {
			msg += " at " + t.Format("Jan 2 · 3:04 PM")
		}
		checkinRespond(w, r, http.StatusConflict, CheckinResult{
			Code: code, Section: section, Row: row, Number: number, Holder: holder,
			CheckedInAt: checkedAt, Msg: msg,
		})
	}
}

// handleCheckinRotate mints a fresh door code; the old link 404s from then on.
func handleCheckinRotate(w http.ResponseWriter, r *http.Request) {
	eventID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	res, err := db.Exec(`UPDATE events SET checkin_code=? WHERE id=?`, "TPK-"+strings.ToUpper(newID(8)), eventID)
	if err != nil {
		httpErr(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/admin")
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ---------- sales report ----------

type reportFilters struct {
	EventID int64
	From    string // YYYY-MM-DD, "" = unbounded
	To      string
}

type ReportRow struct {
	EventID           int64
	Artist            string
	Title             string
	StartsAt          string
	Sold              int64
	Held              int64
	Refunded          int64
	PromoOrders       int64
	GrossCents        int64
	DiscountCents     int64
	NetCents          int64
	CheckedIn         int64
	SoldAllTime       int64
	CheckinCode       string
	Transfers         int64
	ResaleCount       int64
	ResaleVolumeCents int64
	ResaleFeesCents   int64
}

type reportView struct {
	Rows   []ReportRow
	Total  ReportRow
	Events []Event
	F      reportFilters
	CSVURL string
}

func parseReportFilters(r *http.Request) reportFilters {
	var f reportFilters
	f.EventID, _ = strconv.ParseInt(r.URL.Query().Get("event_id"), 10, 64)
	for _, p := range []struct {
		name string
		dst  *string
	}{
		{"from", &f.From},
		{"to", &f.To},
	} {
		v := r.URL.Query().Get(p.name)
		if _, err := time.Parse("2006-01-02", v); err == nil {
			*p.dst = v
		}
	}
	return f
}

// bounds returns the [lo, hi) RFC3339 window for the order-date filter.
func (f reportFilters) bounds() (string, string) {
	lo, hi := "0001-01-01T00:00:00Z", "9999-12-31T23:59:59Z"
	if f.From != "" {
		lo = f.From + "T00:00:00Z"
	}
	if f.To != "" {
		hi = f.To + "T23:59:59Z"
	}
	return lo, hi
}

// buildReport computes per-event sales metrics. Order-date filters apply to
// created_at for sold/revenue/promo metrics and refunded_at for refunds;
// "held" is always the live count.
func buildReport(f reportFilters) (reportView, error) {
	rows, err := db.Query(`SELECT id,artist,title,venue,city,starts_at,description,gradient,checkin_code FROM events ORDER BY starts_at`)
	if err != nil {
		return reportView{}, err
	}
	var events []Event
	doorCodes := map[int64]string{}
	for rows.Next() {
		var e Event
		var door string
		if err := rows.Scan(&e.ID, &e.Artist, &e.Title, &e.Venue, &e.City, &e.StartsAt, &e.Description, &e.Gradient, &door); err != nil {
			rows.Close()
			return reportView{}, err
		}
		doorCodes[e.ID] = door
		events = append(events, e)
	}
	rows.Close()

	lo, hi := f.bounds()
	// Multi-event orders split their discount across seats proportionally:
	// each seat contributes price * discount_fraction of the order's discount.
	v := reportView{Events: events, F: f}
	for _, e := range events {
		if f.EventID != 0 && f.EventID != e.ID {
			continue
		}
		var row ReportRow
		row.EventID, row.Artist, row.Title, row.StartsAt = e.ID, e.Artist, e.Title, e.StartsAt
		err := db.QueryRow(`
			SELECT
			  (SELECT COUNT(*) FROM seats WHERE event_id=? AND status='held'),
			  (SELECT COUNT(*) FROM tickets t JOIN orders o ON o.id=t.order_id JOIN seats s ON s.id=t.seat_id
			    WHERE s.event_id=? AND o.status='paid' AND t.kind<>'resale' AND o.created_at>=? AND o.created_at<=?),
			  COALESCE((SELECT SUM(t.price_cents) FROM tickets t JOIN orders o ON o.id=t.order_id JOIN seats s ON s.id=t.seat_id
			    WHERE s.event_id=? AND o.status='paid' AND t.kind<>'resale' AND o.created_at>=? AND o.created_at<=?),0),
			  COALESCE((SELECT ROUND(SUM(t.price_cents * CAST(o.discount_cents+o.group_discount_cents AS REAL)/NULLIF(o.total_cents+o.discount_cents+o.group_discount_cents,0)))
			    FROM tickets t JOIN orders o ON o.id=t.order_id JOIN seats s ON s.id=t.seat_id
			    WHERE s.event_id=? AND o.status='paid' AND t.kind<>'resale' AND o.created_at>=? AND o.created_at<=?),0),
			  (SELECT COUNT(*) FROM tickets t JOIN orders o ON o.id=t.order_id JOIN seats s ON s.id=t.seat_id
			    WHERE s.event_id=? AND o.status='refunded' AND o.refunded_at>=? AND o.refunded_at<=?),
			  (SELECT COUNT(DISTINCT o.id) FROM orders o JOIN tickets t ON t.order_id=o.id JOIN seats s ON s.id=t.seat_id
			    WHERE s.event_id=? AND o.promo_code<>'' AND o.status='paid' AND o.created_at>=? AND o.created_at<=?),
			  (SELECT COUNT(*) FROM tickets t JOIN orders o ON o.id=t.order_id JOIN seats s ON s.id=t.seat_id
			    WHERE s.event_id=? AND o.status='paid' AND t.resold=0 AND t.checked_in_at IS NOT NULL),
			  (SELECT COUNT(*) FROM tickets t JOIN orders o ON o.id=t.order_id JOIN seats s ON s.id=t.seat_id
			    WHERE s.event_id=? AND o.status='paid' AND t.kind<>'resale'),
			  (SELECT COUNT(*) FROM transfers tr JOIN tickets t ON t.id=tr.ticket_id JOIN seats s ON s.id=t.seat_id
			    WHERE s.event_id=? AND tr.created_at>=? AND tr.created_at<=?),
			  (SELECT COUNT(*) FROM resales rs WHERE rs.event_id=? AND rs.created_at>=? AND rs.created_at<=?),
			  COALESCE((SELECT SUM(rs.price_cents) FROM resales rs WHERE rs.event_id=? AND rs.created_at>=? AND rs.created_at<=?),0),
			  COALESCE((SELECT SUM(rs.fee_cents) FROM resales rs WHERE rs.event_id=? AND rs.created_at>=? AND rs.created_at<=?),0)`,
			e.ID, e.ID, lo, hi, e.ID, lo, hi, e.ID, lo, hi, e.ID, lo, hi, e.ID, lo, hi, e.ID, e.ID,
			e.ID, lo, hi, e.ID, lo, hi, e.ID, lo, hi, e.ID, lo, hi).
			Scan(&row.Held, &row.Sold, &row.GrossCents, &row.DiscountCents, &row.Refunded, &row.PromoOrders,
				&row.CheckedIn, &row.SoldAllTime, &row.Transfers, &row.ResaleCount, &row.ResaleVolumeCents, &row.ResaleFeesCents)
		if err != nil {
			return reportView{}, err
		}
		row.NetCents = row.GrossCents - row.DiscountCents
		row.CheckinCode = doorCodes[e.ID]
		v.Rows = append(v.Rows, row)
		t := &v.Total
		t.Sold += row.Sold
		t.Held += row.Held
		t.Refunded += row.Refunded
		t.PromoOrders += row.PromoOrders
		t.GrossCents += row.GrossCents
		t.DiscountCents += row.DiscountCents
		t.NetCents += row.NetCents
		t.CheckedIn += row.CheckedIn
		t.SoldAllTime += row.SoldAllTime
		t.Transfers += row.Transfers
		t.ResaleCount += row.ResaleCount
		t.ResaleVolumeCents += row.ResaleVolumeCents
		t.ResaleFeesCents += row.ResaleFeesCents
	}
	q := make(url.Values)
	if f.EventID != 0 {
		q.Set("event_id", strconv.FormatInt(f.EventID, 10))
	}
	if f.From != "" {
		q.Set("from", f.From)
	}
	if f.To != "" {
		q.Set("to", f.To)
	}
	v.CSVURL = "/admin/report.csv?" + q.Encode()
	return v, nil
}

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	v, err := buildReport(parseReportFilters(r))
	if err != nil {
		httpErr(w, err)
		return
	}
	render(w, "admin.html", v)
}

func handleAdminReport(w http.ResponseWriter, r *http.Request) {
	v, err := buildReport(parseReportFilters(r))
	if err != nil {
		httpErr(w, err)
		return
	}
	render(w, "report.html", v)
}

func handleAdminCSV(w http.ResponseWriter, r *http.Request) {
	v, err := buildReport(parseReportFilters(r))
	if err != nil {
		httpErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="sales-report.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"event", "starts_at", "tickets_sold", "tickets_checked_in", "seats_held", "tickets_refunded", "promo_orders", "gross", "discounts", "net_revenue", "transfers", "resale_count", "resale_volume", "resale_fees"})
	moneyStr := func(c int64) string { return fmt.Sprintf("%.2f", float64(c)/100) }
	for _, row := range v.Rows {
		cw.Write([]string{
			row.Artist + " — " + row.Title,
			row.StartsAt,
			strconv.FormatInt(row.Sold, 10),
			strconv.FormatInt(row.CheckedIn, 10),
			strconv.FormatInt(row.Held, 10),
			strconv.FormatInt(row.Refunded, 10),
			strconv.FormatInt(row.PromoOrders, 10),
			moneyStr(row.GrossCents),
			moneyStr(row.DiscountCents),
			moneyStr(row.NetCents),
			strconv.FormatInt(row.Transfers, 10),
			strconv.FormatInt(row.ResaleCount, 10),
			moneyStr(row.ResaleVolumeCents),
			moneyStr(row.ResaleFeesCents),
		})
	}
	t := v.Total
	cw.Write([]string{"TOTAL", "", strconv.FormatInt(t.Sold, 10), strconv.FormatInt(t.CheckedIn, 10), strconv.FormatInt(t.Held, 10),
		strconv.FormatInt(t.Refunded, 10), strconv.FormatInt(t.PromoOrders, 10),
		moneyStr(t.GrossCents), moneyStr(t.DiscountCents), moneyStr(t.NetCents),
		strconv.FormatInt(t.Transfers, 10), strconv.FormatInt(t.ResaleCount, 10),
		moneyStr(t.ResaleVolumeCents), moneyStr(t.ResaleFeesCents)})
	cw.Flush()
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
			// a listed seat goes back on the resale map, not to the pool
			_, err := tx.Exec(`UPDATE seats SET status=(CASE WHEN EXISTS(
				SELECT 1 FROM tickets t JOIN orders o ON o.id=t.order_id
				WHERE t.seat_id=seats.id AND t.resold=0 AND o.status='paid' AND t.listing_price_cents>0
			) THEN 'sold' ELSE 'available' END), hold_session=NULL WHERE id=?`, seatID)
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
		db.QueryRow(`SELECT t.listing_price_cents FROM tickets t JOIN orders o ON o.id=t.order_id
			WHERE t.seat_id=? AND t.resold=0 AND o.status='paid' AND t.listing_price_cents>0`, seatID.Int64).Scan(&v.ResaleCents)
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
	rows, err := db.Query(`SELECT id,event_id,section,row,number,price_cents,status,COALESCE(hold_session,''),tier, COALESCE((SELECT t2.listing_price_cents FROM tickets t2 JOIN orders o2 ON o2.id=t2.order_id WHERE t2.seat_id=s.id AND t2.resold=0 AND o2.status='paid' AND t2.listing_price_cents>0),0)
		FROM seats s WHERE event_id=? ORDER BY stage_rank, row, number`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sections []SeatSection
	secIdx := map[string]int{}
	tierSeen := map[string]bool{} // "section|tier" — keep first-seen tier prices
	rowIdx := map[string]int{}    // "section|row" -> index in section.Rows
	for rows.Next() {
		var s Seat
		if err := rows.Scan(&s.ID, &s.EventID, &s.Section, &s.Row, &s.Number, &s.PriceCents, &s.Status, &s.HoldSession, &s.Tier, &s.ResaleCents); err != nil {
			return nil, err
		}
		s.Viewer = sid
		si, ok := secIdx[s.Section]
		if !ok {
			sections = append(sections, SeatSection{Name: s.Section, PriceCents: s.PriceCents})
			si = len(sections) - 1
			secIdx[s.Section] = si
		}
		if tkey := s.Section + "|" + s.Tier; !tierSeen[tkey] {
			tierSeen[tkey] = true
			sections[si].Tiers = append(sections[si].Tiers, SeatTier{Name: s.Tier, PriceCents: s.PriceCents})
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
	rows, err := db.Query(`SELECT id,event_id,section,row,number,price_cents,status,COALESCE(hold_session,''),tier, COALESCE((SELECT t2.listing_price_cents FROM tickets t2 JOIN orders o2 ON o2.id=t2.order_id WHERE t2.seat_id=s.id AND t2.resold=0 AND o2.status='paid' AND t2.listing_price_cents>0),0)
		FROM seats s WHERE event_id=? AND status='held' AND hold_session=? ORDER BY stage_rank,row,number`, eventID, sid)
	if err != nil {
		return CartView{}, err
	}
	defer rows.Close()
	cart := CartView{EventID: eventID, Max: maxTicketsPerEvent}
	for rows.Next() {
		var s Seat
		if err := rows.Scan(&s.ID, &s.EventID, &s.Section, &s.Row, &s.Number, &s.PriceCents, &s.Status, &s.HoldSession, &s.Tier, &s.ResaleCents); err != nil {
			return CartView{}, err
		}
		cart.Seats = append(cart.Seats, s)
		cart.TotalCents += s.CartCents()
		if s.ResaleCents > 0 {
			cart.HasResale = true
		}
	}
	if rows.Err() != nil {
		return CartView{}, rows.Err()
	}
	var normal []Seat
	for _, s := range cart.Seats {
		if s.ResaleCents == 0 {
			normal = append(normal, s)
		}
	}
	cart.DealCents = groupDiscount(normal)
	cart.TotalCents -= cart.DealCents
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
	rows, err := db.Query(`SELECT s.id,s.event_id,s.section,s.row,s.number,s.price_cents,s.status,COALESCE(s.hold_session,''),s.tier,
		       e.artist || ' — ' || e.title, COALESCE((SELECT t2.listing_price_cents FROM tickets t2 JOIN orders o2 ON o2.id=t2.order_id WHERE t2.seat_id=s.id AND t2.resold=0 AND o2.status='paid' AND t2.listing_price_cents>0),0)
		FROM seats s JOIN events e ON e.id = s.event_id
		WHERE s.status='held' AND s.hold_session=? ORDER BY s.event_id,s.stage_rank,s.row,s.number`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seat
	for rows.Next() {
		var s Seat
		if err := rows.Scan(&s.ID, &s.EventID, &s.Section, &s.Row, &s.Number, &s.PriceCents, &s.Status, &s.HoldSession, &s.Tier, &s.EventName, &s.ResaleCents); err != nil {
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

// ---------- tickets: page, transfer, resale listing ----------
//
// The code is the credential: whoever holds the current code of a ticket is
// its holder, the same way a door code is the door's credential. A transfer
// retires the old code into the transfers table and mints a new one; a resale
// leaves the code dead on the ticket row with resold=1.

// ticketState is one ticket as the page and JSON view see it.
type ticketState struct {
	ID          int64
	Code        string
	OrderID     int64
	SeatID      int64
	EventID     int64
	Section     string
	Row         string
	Number      int64
	Tier        string
	FaceCents   int64
	HolderName  string
	HolderEmail string
	OrderStatus string
	CheckedInAt string
	ListedCents int64
	Resold      bool
	ResaleCents int64
	Transfers   int64
	Artist      string
	Title       string
	Venue       string
	City        string
	StartsAt    string
}

// ticketForCode loads a live ticket row by its (normalized) code.
func ticketForCode(code string) (ticketState, error) {
	var t ticketState
	err := db.QueryRow(`SELECT t.id, t.code, t.order_id, t.seat_id, s.event_id,
		s.section, s.row, s.number, s.tier, t.price_cents,
		COALESCE(NULLIF(t.holder_name,''),o.name), COALESCE(NULLIF(t.holder_email,''),o.email),
		o.status, COALESCE(t.checked_in_at,''), t.listing_price_cents, t.resold, t.resale_price_cents,
		(SELECT COUNT(*) FROM transfers tr WHERE tr.ticket_id=t.id),
		e.artist, e.title, e.venue, e.city, e.starts_at
		FROM tickets t
		JOIN seats s ON s.id=t.seat_id
		JOIN orders o ON o.id=t.order_id
		JOIN events e ON e.id=s.event_id
		WHERE t.code=?`, code).
		Scan(&t.ID, &t.Code, &t.OrderID, &t.SeatID, &t.EventID, &t.Section, &t.Row, &t.Number, &t.Tier,
			&t.FaceCents, &t.HolderName, &t.HolderEmail, &t.OrderStatus, &t.CheckedInAt,
			&t.ListedCents, &t.Resold, &t.ResaleCents, &t.Transfers,
			&t.Artist, &t.Title, &t.Venue, &t.City, &t.StartsAt)
	return t, err
}

// transferToName resolves a dead code to the holder it was transferred to.
func transferToName(code string) (string, bool) {
	var name string
	err := db.QueryRow(`SELECT holder_name FROM transfers WHERE old_code=?`, code).Scan(&name)
	return name, err == nil
}

type holderView struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// holderChain is the buyer first, then every holder the ticket was
// transferred to, oldest first — the last entry is the current holder.
func holderChain(t ticketState) ([]holderView, error) {
	var buyer holderView
	if err := db.QueryRow(`SELECT name, email FROM orders WHERE id=?`, t.OrderID).Scan(&buyer.Name, &buyer.Email); err != nil {
		return nil, err
	}
	chain := []holderView{buyer}
	rows, err := db.Query(`SELECT holder_name, holder_email FROM transfers WHERE ticket_id=? ORDER BY id`, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h holderView
		if err := rows.Scan(&h.Name, &h.Email); err != nil {
			return nil, err
		}
		chain = append(chain, h)
	}
	return chain, rows.Err()
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// ticketJSON is the wire shape for GET /tickets/{code} and the refusals.
type ticketJSON struct {
	Code              string       `json:"code"`
	Status            string       `json:"status"`
	HolderName        string       `json:"holder_name"`
	HolderEmail       string       `json:"holder_email"`
	EventID           int64        `json:"event_id"`
	Section           string       `json:"section"`
	Row               string       `json:"row"`
	Number            int64        `json:"number"`
	FaceValueCents    int64        `json:"face_value_cents"`
	Transfers         int64        `json:"transfers"`
	Holders           []holderView `json:"holders"`
	CheckedInAt       string       `json:"checked_in_at"`
	ListingPriceCents int64        `json:"listing_price_cents"`
	ResalePriceCents  int64        `json:"resale_price_cents,omitempty"`
	FeeCents          int64        `json:"fee_cents,omitempty"`
	PayoutCents       int64        `json:"payout_cents,omitempty"`
	Message           string       `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ticketRefuse answers an action refusal in the requester's format: plain
// text for browsers, a JSON object carrying the same sentence for scanners.
func ticketRefuse(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if wantsJSON(r) {
		writeJSON(w, status, map[string]any{"code": r.PathValue("code"), "message": msg})
		return
	}
	http.Error(w, msg, status)
}

func ticketRedirect(w http.ResponseWriter, r *http.Request, code string) {
	url := "/tickets/" + code
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", url)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// resolveTicket maps a request code to a live ticket, a transferred dead code,
// or nothing. The second return is the holder a transferred code went to.
func resolveTicket(raw string) (ticketState, string, error) {
	code := normalizeTicketCode(raw)
	t, err := ticketForCode(code)
	if err == nil {
		return t, "", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ticketState{}, "", err
	}
	if name, ok := transferToName(code); ok {
		return ticketState{Code: code}, name, nil
	}
	return ticketState{}, "", sql.ErrNoRows
}

func handleTicketPage(w http.ResponseWriter, r *http.Request) {
	t, transferredTo, err := resolveTicket(r.PathValue("code"))
	if errors.Is(err, sql.ErrNoRows) {
		if wantsJSON(r) {
			writeJSON(w, http.StatusNotFound, ticketJSON{Code: normalizeTicketCode(r.PathValue("code")), Status: "not_found", Message: "Unknown ticket code."})
			return
		}
		http.Error(w, "Unknown ticket code.", http.StatusNotFound)
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	if transferredTo != "" {
		msg := "This ticket was transferred to " + transferredTo + "."
		if wantsJSON(r) {
			writeJSON(w, http.StatusGone, ticketJSON{Code: t.Code, Status: "transferred", HolderName: transferredTo, Message: msg})
			return
		}
		http.Error(w, msg, http.StatusGone)
		return
	}
	if t.Resold {
		fee := (t.ResaleCents*5 + 50) / 100
		if wantsJSON(r) {
			writeJSON(w, http.StatusGone, ticketJSON{Code: t.Code, Status: "resold",
				ResalePriceCents: t.ResaleCents, FeeCents: fee, PayoutCents: t.ResaleCents - fee,
				Message: "This ticket was sold on resale."})
			return
		}
		http.Error(w, "This ticket was sold on resale.", http.StatusGone)
		return
	}
	if t.OrderStatus != "paid" {
		if wantsJSON(r) {
			writeJSON(w, http.StatusGone, ticketJSON{Code: t.Code, Status: "refunded", Message: "This ticket was refunded."})
			return
		}
		http.Error(w, "This ticket was refunded.", http.StatusGone)
		return
	}
	chain, err := holderChain(t)
	if err != nil {
		httpErr(w, err)
		return
	}
	status := "active"
	if t.ListedCents > 0 {
		status = "listed"
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, ticketJSON{Code: t.Code, Status: status,
			HolderName: t.HolderName, HolderEmail: t.HolderEmail, EventID: t.EventID,
			Section: t.Section, Row: t.Row, Number: t.Number, FaceValueCents: t.FaceCents,
			Transfers: t.Transfers, Holders: chain, CheckedInAt: t.CheckedInAt,
			ListingPriceCents: t.ListedCents})
		return
	}
	// HTML: forms when the holder can act, the matching sentence when not.
	var transferNote, listNote string
	switch {
	case t.CheckedInAt != "":
		transferNote = "This ticket has already been checked in and cannot be transferred."
		listNote = "This ticket has already been checked in and cannot be listed."
	case t.Transfers >= 3:
		transferNote = "This ticket has reached the limit of 3 transfers."
	}
	render(w, "ticket.html", struct {
		T            ticketState
		Holders      []holderView
		Listed       bool
		CanTransfer  bool
		CanList      bool
		TransferNote string
		ListNote     string
	}{t, chain, t.ListedCents > 0, transferNote == "" && t.ListedCents == 0, listNote == "" && t.ListedCents == 0, transferNote, listNote})
}

func handleTicketTransfer(w http.ResponseWriter, r *http.Request) {
	t, transferredTo, err := resolveTicket(r.PathValue("code"))
	if errors.Is(err, sql.ErrNoRows) {
		ticketRefuse(w, r, http.StatusNotFound, "Unknown ticket code.")
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	switch {
	case transferredTo != "":
		ticketRefuse(w, r, http.StatusGone, "This ticket was transferred to "+transferredTo+".")
		return
	case t.Resold:
		ticketRefuse(w, r, http.StatusGone, "This ticket was sold on resale.")
		return
	case t.OrderStatus != "paid":
		ticketRefuse(w, r, http.StatusGone, "This ticket was refunded and cannot be transferred.")
		return
	case t.CheckedInAt != "":
		ticketRefuse(w, r, http.StatusConflict, "This ticket has already been checked in and cannot be transferred.")
		return
	case t.ListedCents > 0:
		ticketRefuse(w, r, http.StatusConflict, "This ticket is listed for resale. Delist it before transferring.")
		return
	case t.Transfers >= 3:
		ticketRefuse(w, r, http.StatusConflict, "This ticket has reached the limit of 3 transfers.")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	email := strings.TrimSpace(r.FormValue("email"))
	if name == "" || !strings.Contains(email, "@") {
		ticketRefuse(w, r, http.StatusUnprocessableEntity, "Enter a name and a valid email address for the new holder.")
		return
	}
	if strings.EqualFold(email, t.HolderEmail) {
		ticketRefuse(w, r, http.StatusUnprocessableEntity, "This ticket already belongs to that email address.")
		return
	}
	newCode := "TP-" + strings.ToUpper(newID(5))
	tx, err := db.Begin()
	if err != nil {
		httpErr(w, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO transfers (ticket_id,old_code,new_code,holder_name,holder_email,created_at)
		VALUES (?,?,?,?,?,strftime('%Y-%m-%dT%H:%M:%SZ','now'))`, t.ID, t.Code, newCode, name, email); err != nil {
		httpErr(w, err)
		return
	}
	if _, err := tx.Exec(`UPDATE tickets SET code=?, holder_name=?, holder_email=? WHERE id=?`, newCode, name, email, t.ID); err != nil {
		httpErr(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpErr(w, err)
		return
	}
	ticketRedirect(w, r, newCode)
}

var dollarsRx = regexp.MustCompile(`^(\d+)(?:\.(\d{1,2}))?$`)

// parseDollars turns "130", "130.5" or "130.00" into cents.
func parseDollars(s string) (int64, bool) {
	m := dollarsRx.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	whole, _ := strconv.ParseInt(m[1], 10, 64)
	cents := whole * 100
	if m[2] != "" {
		frac, _ := strconv.ParseInt(m[2]+"0"[:2-len(m[2])], 10, 64)
		cents += frac
	}
	return cents, cents > 0
}

func handleTicketList(w http.ResponseWriter, r *http.Request) {
	t, transferredTo, err := resolveTicket(r.PathValue("code"))
	if errors.Is(err, sql.ErrNoRows) {
		ticketRefuse(w, r, http.StatusNotFound, "Unknown ticket code.")
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	switch {
	case transferredTo != "":
		ticketRefuse(w, r, http.StatusGone, "This ticket was transferred to "+transferredTo+".")
		return
	case t.Resold:
		ticketRefuse(w, r, http.StatusGone, "This ticket was sold on resale.")
		return
	case t.OrderStatus != "paid":
		ticketRefuse(w, r, http.StatusGone, "This ticket was refunded and cannot be listed.")
		return
	case t.CheckedInAt != "":
		ticketRefuse(w, r, http.StatusConflict, "This ticket has already been checked in and cannot be listed.")
		return
	case t.ListedCents > 0:
		ticketRefuse(w, r, http.StatusConflict, "This ticket is already listed for resale.")
		return
	}
	price, ok := parseDollars(r.FormValue("price"))
	if !ok {
		ticketRefuse(w, r, http.StatusUnprocessableEntity, "Enter a resale price in dollars, like 120.00.")
		return
	}
	cap := t.FaceCents * 110 / 100
	if price > cap {
		ticketRefuse(w, r, http.StatusUnprocessableEntity, fmt.Sprintf("Resale price cannot be more than $%.2f, which is 110%% of the $%.2f face value.", float64(cap)/100, float64(t.FaceCents)/100))
		return
	}
	tx, err := db.Begin()
	if err != nil {
		httpErr(w, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE tickets SET listing_price_cents=? WHERE id=?`, price, t.ID); err != nil {
		httpErr(w, err)
		return
	}
	// on a sold out event the listing is offered to the waitlist first
	soldOut, err := eventSoldOut(t.EventID)
	if err == nil && soldOut {
		err = offerSeat(tx, t.SeatID, t.EventID)
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpErr(w, err)
		return
	}
	ticketRedirect(w, r, t.Code)
}

func handleTicketDelist(w http.ResponseWriter, r *http.Request) {
	t, transferredTo, err := resolveTicket(r.PathValue("code"))
	if errors.Is(err, sql.ErrNoRows) {
		ticketRefuse(w, r, http.StatusNotFound, "Unknown ticket code.")
		return
	}
	if err != nil {
		httpErr(w, err)
		return
	}
	switch {
	case transferredTo != "":
		ticketRefuse(w, r, http.StatusGone, "This ticket was transferred to "+transferredTo+".")
		return
	case t.Resold:
		ticketRefuse(w, r, http.StatusGone, "This ticket was sold on resale.")
		return
	case t.OrderStatus != "paid":
		ticketRefuse(w, r, http.StatusGone, "This ticket was refunded and cannot be delisted.")
		return
	case t.ListedCents == 0:
		ticketRefuse(w, r, http.StatusConflict, "This ticket is not listed for resale.")
		return
	}
	var heldBy int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM seats WHERE id=? AND status='held'`, t.SeatID).Scan(&heldBy); err != nil {
		httpErr(w, err)
		return
	}
	if heldBy > 0 {
		ticketRefuse(w, r, http.StatusConflict, "This ticket is in a buyer's cart and cannot be delisted right now.")
		return
	}
	if _, err := db.Exec(`UPDATE tickets SET listing_price_cents=0 WHERE id=?`, t.ID); err != nil {
		httpErr(w, err)
		return
	}
	ticketRedirect(w, r, t.Code)
}
