package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"babyshower/backend/internal/store"
)

// Store es el contrato que api usa; *store.DB lo implementa.
type Store interface {
	GetEvent(ctx context.Context) (store.Event, error)
	UpdateEvent(ctx context.Context, ev store.Event) error
	ListGifts(ctx context.Context) ([]store.GiftPublic, error)
	ListGiftsAdmin(ctx context.Context) ([]store.GiftAdmin, error)
	CreateGift(ctx context.Context, in store.GiftInput) (store.GiftPublic, error)
	UpdateGift(ctx context.Context, id int, in store.GiftInput) (store.GiftPublic, error)
	DeleteGift(ctx context.Context, id int) error
	ClaimGift(ctx context.Context, giftID int, name, phone string) (string, error)
	ReleaseGift(ctx context.Context, giftID int, token string) error
	CreateRsvp(ctx context.Context, in store.RsvpInput) error
	ListRsvps(ctx context.Context) ([]store.Rsvp, error)
}

type Server struct {
	store        Store
	dist         fs.FS
	fingerprints map[string]struct{}
	limiter      *limiter
}

// New construye el servidor. fingerprints son los SHA256 (hex) de los
// certificados de cliente permitidos para el admin; si está vacío no se
// exige el header (modo desarrollo local).
func New(s Store, fingerprints []string, dist fs.FS) *Server {
	set := make(map[string]struct{}, len(fingerprints))
	for _, f := range fingerprints {
		f = strings.ToLower(strings.TrimSpace(f))
		if f != "" {
			set[f] = struct{}{}
		}
	}
	return &Server{store: s, dist: dist, fingerprints: set, limiter: newLimiter(10, time.Minute)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/event", s.handleGetEvent)
	mux.HandleFunc("GET /api/gifts", s.handleListGifts)
	mux.HandleFunc("POST /api/gifts/{id}/claim", s.handleClaimGift)
	mux.HandleFunc("DELETE /api/gifts/{id}/claim", s.handleReleaseGift)
	mux.HandleFunc("POST /api/rsvps", s.handleCreateRsvp)

	mux.HandleFunc("GET /api/admin/gifts", s.handleListGiftsAdmin)
	mux.HandleFunc("POST /api/admin/gifts", s.handleCreateGift)
	mux.HandleFunc("PUT /api/admin/gifts/{id}", s.handleUpdateGift)
	mux.HandleFunc("DELETE /api/admin/gifts/{id}", s.handleDeleteGift)
	mux.HandleFunc("GET /api/admin/rsvps", s.handleListRsvps)
	mux.HandleFunc("GET /api/admin/event", s.handleGetEvent)
	mux.HandleFunc("PUT /api/admin/event", s.handleUpdateEvent)

	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /admin\n"))
	})

	mux.HandleFunc("/", s.serveStatic)

	return s.adminGate(mux)
}

// ---------- middleware ----------

// adminGate exige el header Cf-Client-Cert-Sha256 (reenviado por Cloudflare
// cuando la conexión presenta un certificado de cliente válido) para toda
// ruta /admin o /api/admin, si hay fingerprints configurados.
func (s *Server) adminGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.fingerprints) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		p := r.URL.Path
		if p == "/admin" || strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/api/admin") {
			fp := strings.ToLower(strings.TrimSpace(r.Header.Get("Cf-Client-Cert-Sha256")))
			if _, ok := s.fingerprints[fp]; !ok {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	if err := dec.Decode(&v); err != nil {
		writeErr(w, http.StatusBadRequest, "cuerpo JSON inválido")
		return v, false
	}
	return v, true
}

func pathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return 0, false
	}
	return id, true
}

func trimPtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

func validURL(u *string) bool {
	if u == nil || *u == "" {
		return true
	}
	parsed, err := url.Parse(*u)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// ---------- rate limiting (por IP, POST) ----------

type limiter struct {
	mu    sync.Mutex
	hits  map[string][]time.Time
	limit int
	window time.Duration
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{hits: map[string][]time.Time{}, limit: limit, window: window}
}

func (l *limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	hits := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			hits = append(hits, t)
		}
	}
	if len(hits) >= l.limit {
		l.hits[key] = hits
		return false
	}
	l.hits[key] = append(hits, now)
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- handlers públicos ----------

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := s.store.GetEvent(r.Context())
	if err != nil {
		log.Printf("get event: %v", err)
		writeErr(w, http.StatusInternalServerError, "error interno")
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) handleListGifts(w http.ResponseWriter, r *http.Request) {
	gifts, err := s.store.ListGifts(r.Context())
	if err != nil {
		log.Printf("list gifts: %v", err)
		writeErr(w, http.StatusInternalServerError, "error interno")
		return
	}
	if gifts == nil {
		gifts = []store.GiftPublic{}
	}
	writeJSON(w, http.StatusOK, gifts)
}

func (s *Server) handleClaimGift(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "demasiadas peticiones, espera un momento")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	body, ok := readBody[struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
	}](w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(body.Name)
	phone := strings.TrimSpace(body.Phone)
	if name == "" || len(name) > 120 {
		writeErr(w, http.StatusBadRequest, "nombre requerido (máx 120)")
		return
	}
	if phone == "" || len(phone) > 30 {
		writeErr(w, http.StatusBadRequest, "teléfono requerido (máx 30)")
		return
	}
	token, err := s.store.ClaimGift(r.Context(), id, name, phone)
	switch {
	case errors.Is(err, store.ErrAlreadyClaimed):
		writeErr(w, http.StatusConflict, "este regalo ya está reservado")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "regalo no encontrado")
	case err != nil:
		log.Printf("claim gift %d: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "error interno")
	default:
		writeJSON(w, http.StatusCreated, map[string]string{"claim_token": token})
	}
}

func (s *Server) handleReleaseGift(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "demasiadas peticiones, espera un momento")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	token := strings.TrimSpace(r.Header.Get("X-Claim-Token"))
	if token == "" {
		writeErr(w, http.StatusBadRequest, "falta token")
		return
	}
	err := s.store.ReleaseGift(r.Context(), id, token)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "reserva no encontrada")
	case err != nil:
		log.Printf("release gift %d: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "error interno")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleCreateRsvp(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "demasiadas peticiones, espera un momento")
		return
	}
	body, ok := readBody[struct {
		Name     string  `json:"name"`
		Phone    *string `json:"phone"`
		Attending bool   `json:"attending"`
		Guests   int     `json:"guests"`
		Message  *string `json:"message"`
	}](w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 120 {
		writeErr(w, http.StatusBadRequest, "nombre requerido (máx 120)")
		return
	}
	phone := trimPtr(body.Phone)
	if phone != nil && (len(*phone) == 0 || len(*phone) > 30) {
		phone = nil
	}
	guests := body.Guests
	if !body.Attending {
		guests = 0
	}
	if guests < 0 || guests > 10 {
		writeErr(w, http.StatusBadRequest, "acompañantes entre 0 y 10")
		return
	}
	message := trimPtr(body.Message)
	if message != nil {
		if len(*message) == 0 || len(*message) > 500 {
			message = nil
		}
	}
	err := s.store.CreateRsvp(r.Context(), store.RsvpInput{
		Name: name, Phone: phone, Attending: body.Attending, Guests: guests, Message: message,
	})
	if err != nil {
		log.Printf("create rsvp: %v", err)
		writeErr(w, http.StatusInternalServerError, "error interno")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

// ---------- handlers admin ----------

func (s *Server) handleListGiftsAdmin(w http.ResponseWriter, r *http.Request) {
	gifts, err := s.store.ListGiftsAdmin(r.Context())
	if err != nil {
		log.Printf("list gifts admin: %v", err)
		writeErr(w, http.StatusInternalServerError, "error interno")
		return
	}
	if gifts == nil {
		gifts = []store.GiftAdmin{}
	}
	writeJSON(w, http.StatusOK, gifts)
}

func (s *Server) handleCreateGift(w http.ResponseWriter, r *http.Request) {
	in, ok := s.parseGiftInput(w, r)
	if !ok {
		return
	}
	g, err := s.store.CreateGift(r.Context(), in)
	if err != nil {
		log.Printf("create gift: %v", err)
		writeErr(w, http.StatusInternalServerError, "error interno")
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) handleUpdateGift(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in, ok := s.parseGiftInput(w, r)
	if !ok {
		return
	}
	g, err := s.store.UpdateGift(r.Context(), id, in)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "regalo no encontrado")
	case err != nil:
		log.Printf("update gift %d: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "error interno")
	default:
		writeJSON(w, http.StatusOK, g)
	}
}

func (s *Server) handleDeleteGift(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := s.store.DeleteGift(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "regalo no encontrado")
	case err != nil:
		log.Printf("delete gift %d: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "error interno")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) parseGiftInput(w http.ResponseWriter, r *http.Request) (store.GiftInput, bool) {
	body, ok := readBody[struct {
		Position int     `json:"position"`
		Title    string  `json:"title"`
		Note     string  `json:"note"`
		URL      *string `json:"url"`
	}](w, r)
	if !ok {
		return store.GiftInput{}, false
	}
	title := strings.TrimSpace(body.Title)
	if title == "" || len(title) > 160 {
		writeErr(w, http.StatusBadRequest, "título requerido (máx 160)")
		return store.GiftInput{}, false
	}
	note := strings.TrimSpace(body.Note)
	if len(note) > 200 {
		writeErr(w, http.StatusBadRequest, "nota demasiado larga (máx 200)")
		return store.GiftInput{}, false
	}
	u := trimPtr(body.URL)
	if u != nil && len(*u) > 500 {
		writeErr(w, http.StatusBadRequest, "url demasiado larga")
		return store.GiftInput{}, false
	}
	if !validURL(u) {
		writeErr(w, http.StatusBadRequest, "url debe ser http(s)")
		return store.GiftInput{}, false
	}
	if u != nil && len(*u) == 0 {
		u = nil
	}
	pos := body.Position
	if pos < 1 {
		pos = 1
	}
	return store.GiftInput{Position: pos, Title: title, Note: note, URL: u}, true
}

func (s *Server) handleListRsvps(w http.ResponseWriter, r *http.Request) {
	rsvps, err := s.store.ListRsvps(r.Context())
	if err != nil {
		log.Printf("list rsvps: %v", err)
		writeErr(w, http.StatusInternalServerError, "error interno")
		return
	}
	if rsvps == nil {
		rsvps = []store.Rsvp{}
	}
	writeJSON(w, http.StatusOK, rsvps)
}

func (s *Server) handleUpdateEvent(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[struct {
		EventDate    string  `json:"event_date"`
		EventTime    *string `json:"event_time"`
		Place        string  `json:"place"`
		RsvpDeadline *string `json:"rsvp_deadline"`
	}](w, r)
	if !ok {
		return
	}
	if !validDate(body.EventDate) {
		writeErr(w, http.StatusBadRequest, "fecha inválida (YYYY-MM-DD)")
		return
	}
	place := strings.TrimSpace(body.Place)
	if place == "" || len(place) > 300 {
		writeErr(w, http.StatusBadRequest, "lugar requerido (máx 300)")
		return
	}
	var evTime *string
	if body.EventTime != nil && strings.TrimSpace(*body.EventTime) != "" {
		t := strings.TrimSpace(*body.EventTime)
		if !validTime(t) {
			writeErr(w, http.StatusBadRequest, "hora inválida (HH:MM)")
			return
		}
		evTime = &t
	}
	var deadline *string
	if body.RsvpDeadline != nil && strings.TrimSpace(*body.RsvpDeadline) != "" {
		d := strings.TrimSpace(*body.RsvpDeadline)
		if !validDate(d) {
			writeErr(w, http.StatusBadRequest, "fecha límite inválida (YYYY-MM-DD)")
			return
		}
		deadline = &d
	}
	err := s.store.UpdateEvent(r.Context(), store.Event{
		EventDate: body.EventDate, EventTime: evTime, Place: place, RsvpDeadline: deadline,
	})
	if err != nil {
		log.Printf("update event: %v", err)
		writeErr(w, http.StatusInternalServerError, "error interno")
		return
	}
	ev, err := s.store.GetEvent(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "error interno")
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func validDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func validTime(s string) bool {
	_, err := time.Parse("15:04", s)
	return err == nil
}

// ---------- estáticos ----------

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}

	if strings.HasPrefix(p, "_astro/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	if p == "admin" || p == "admin/" || strings.HasPrefix(p, "admin/") {
		s.serveFile(w, r, "admin/index.html")
		return
	}

	s.serveFile(w, r, p)
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := s.dist.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	seeker, ok := f.(io.ReadSeeker)
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, st.ModTime(), seeker)
}
