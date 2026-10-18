package main

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed templates static
var assets embed.FS

var db *sql.DB
var tpl *template.Template

func main() {
	var err error
	db, err = sql.Open("sqlite", "file:tickets.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := migrate(db); err != nil {
		log.Fatal(err)
	}
	if err := seed(db); err != nil {
		log.Fatal(err)
	}

	tpl = parseTemplates()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleIndex)
	mux.HandleFunc("GET /events/{id}", handleEvent)
	mux.HandleFunc("POST /events/{id}/seats/{seatID}/toggle", handleToggleSeat)
	mux.HandleFunc("POST /events/{id}/cart/clear", handleClearCart)
	mux.HandleFunc("POST /events/{id}/best", handlePickBest)
	mux.HandleFunc("POST /events/{id}/waitlist", handleJoinWaitlist)
	mux.HandleFunc("POST /events/{id}/waitlist/leave", handleLeaveWaitlist)
	mux.HandleFunc("POST /orders/{id}/refund", handleRefund)
	mux.HandleFunc("GET /checkout", handleCheckoutPage)
	mux.HandleFunc("POST /checkout/promo", handleApplyPromo)
	mux.HandleFunc("POST /checkout", handleCheckout)
	mux.HandleFunc("GET /orders/{id}", handleOrder)
	mux.HandleFunc("GET /admin", handleAdmin)
	mux.HandleFunc("GET /admin/report", handleAdminReport)
	mux.HandleFunc("GET /admin/report.csv", handleAdminCSV)
	mux.HandleFunc("POST /admin/events/{id}/checkin/rotate", handleCheckinRotate)
	mux.HandleFunc("GET /checkin/{doorCode}", handleCheckinPage)
	mux.HandleFunc("POST /checkin/{doorCode}", handleCheckinScan)
	mux.HandleFunc("GET /checkin/{doorCode}/status", handleCheckinStatus)
	mux.HandleFunc("GET /tickets/{code}", handleTicketPage)
	mux.HandleFunc("POST /tickets/{code}/transfer", handleTicketTransfer)
	mux.HandleFunc("POST /tickets/{code}/list", handleTicketList)
	mux.HandleFunc("POST /tickets/{code}/delist", handleTicketDelist)
	mux.Handle("GET /static/", http.FileServerFS(assets))

	if v := strings.TrimSpace(os.Getenv("OFFER_TTL_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offerTTL = time.Duration(n) * time.Second
		}
	}

	addr := ":8080"
	log.Printf("ticketpunch listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, sessionMiddleware(mux)))
}

func parseTemplates() *template.Template {
	return template.Must(template.New("").Funcs(template.FuncMap{
		"money": func(cents int64) string { return fmt.Sprintf("$%.2f", float64(cents)/100) },
		"qr":    qrSVG,
		"datefmt": func(iso string) string {
			t, err := time.Parse(time.RFC3339, iso)
			if err != nil {
				return iso
			}
			return t.Format("Mon, Jan 2 · 3:04 PM")
		},
		"div2": func(n int) int { return n / 2 },
		"pct": func(a, b int64) int64 {
			if b <= 0 {
				return 0
			}
			return a * 100 / b
		},
		"sub": func(a, b int) int { return a - b },
		"seq": func(lo, hi int) []int {
			out := make([]int, 0, hi-lo+1)
			for i := lo; i <= hi; i++ {
				out = append(out, i)
			}
			return out
		},
	}).ParseFS(assets, "templates/*.html"))
}

func sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("sid"); err == nil && c.Value != "" {
			next.ServeHTTP(w, r.WithContext(withSession(r.Context(), c.Value)))
			return
		}
		sid := newID(16)
		http.SetCookie(w, &http.Cookie{
			Name:     "sid",
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   60 * 60 * 24 * 7,
		})
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sid)))
	})
}

func newID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(b)
}
