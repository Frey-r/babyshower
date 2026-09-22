package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"babyshower/backend/internal/auth"
	"babyshower/backend/internal/store"
)

type fakeStore struct {
	event    store.Event
	gifts    map[int]store.GiftAdmin
	claims   map[int]store.GiftClaimInfo
	tokens   map[string]int
	rsvps    []store.Rsvp
	sessions map[string]sessionRow
	nextID   int
}

type sessionRow struct {
	email string
	role  string
	exp   time.Time
}

func newFakeStore() *fakeStore {
	g := store.GiftPublic{ID: 1, Position: 1, Title: "Silla"}
	return &fakeStore{
		event:    store.Event{EventDate: "2026-10-03", Place: "Casa"},
		gifts:    map[int]store.GiftAdmin{1: {GiftPublic: g}},
		claims:   map[int]store.GiftClaimInfo{},
		tokens:   map[string]int{},
		rsvps:    []store.Rsvp{},
		sessions: map[string]sessionRow{},
		nextID:   2,
	}
}

func (f *fakeStore) GetEvent(context.Context) (store.Event, error) { return f.event, nil }

func (f *fakeStore) UpdateEvent(_ context.Context, ev store.Event) error { f.event = ev; return nil }

func (f *fakeStore) ListGifts(context.Context) ([]store.GiftPublic, error) {
	out := []store.GiftPublic{}
	for _, g := range f.gifts {
		g.Claimed = g.Claim != nil
		out = append(out, g.GiftPublic)
	}
	return out, nil
}

func (f *fakeStore) ListGiftsAdmin(context.Context) ([]store.GiftAdmin, error) {
	out := []store.GiftAdmin{}
	for _, g := range f.gifts {
		out = append(out, g)
	}
	return out, nil
}

func (f *fakeStore) CreateGift(_ context.Context, in store.GiftInput) (store.GiftPublic, error) {
	g := store.GiftPublic{ID: f.nextID, Position: in.Position, Title: in.Title, Note: in.Note, URL: in.URL}
	f.gifts[f.nextID] = store.GiftAdmin{GiftPublic: g}
	f.nextID++
	return g, nil
}

func (f *fakeStore) UpdateGift(_ context.Context, id int, in store.GiftInput) (store.GiftPublic, error) {
	g, ok := f.gifts[id]
	if !ok {
		return store.GiftPublic{}, store.ErrNotFound
	}
	g.Position, g.Title, g.Note, g.URL = in.Position, in.Title, in.Note, in.URL
	f.gifts[id] = g
	return g.GiftPublic, nil
}

func (f *fakeStore) DeleteGift(_ context.Context, id int) error {
	if _, ok := f.gifts[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.gifts, id)
	delete(f.claims, id)
	return nil
}

func (f *fakeStore) ClaimGift(_ context.Context, giftID int, name, phone string) (string, error) {
	if _, ok := f.gifts[giftID]; !ok {
		return "", store.ErrNotFound
	}
	if _, ok := f.claims[giftID]; ok {
		return "", store.ErrAlreadyClaimed
	}
	token := "tok-" + name
	f.claims[giftID] = store.GiftClaimInfo{GuestName: name, GuestPhone: phone}
	f.tokens[token] = giftID
	g := f.gifts[giftID]
	g.Claim = &store.GiftClaimInfo{GuestName: name, GuestPhone: phone}
	f.gifts[giftID] = g
	return token, nil
}

func (f *fakeStore) ReleaseGift(_ context.Context, giftID int, token string) error {
	if f.tokens[token] != giftID {
		return store.ErrNotFound
	}
	delete(f.claims, giftID)
	delete(f.tokens, token)
	g := f.gifts[giftID]
	g.Claim = nil
	f.gifts[giftID] = g
	return nil
}

func (f *fakeStore) CreateRsvp(_ context.Context, in store.RsvpInput) error {
	f.rsvps = append(f.rsvps, store.Rsvp{ID: len(f.rsvps) + 1, Name: in.Name, Phone: in.Phone, Attending: in.Attending, Guests: in.Guests, Message: in.Message})
	return nil
}

func (f *fakeStore) ListRsvps(context.Context) ([]store.Rsvp, error) { return f.rsvps, nil }

func (f *fakeStore) CreateSession(_ context.Context, id, email, role string, exp time.Time) error {
	f.sessions[id] = sessionRow{email: strings.ToLower(email), role: strings.ToLower(role), exp: exp}
	return nil
}

func (f *fakeStore) GetSession(_ context.Context, id string) (string, string, error) {
	s, ok := f.sessions[id]
	if !ok || time.Now().After(s.exp) {
		return "", "", store.ErrNotFound
	}
	return s.email, s.role, nil
}

func (f *fakeStore) DeleteSession(_ context.Context, id string) error {
	delete(f.sessions, id)
	return nil
}

// ---------- helpers ----------

func testFS() fs.FS {
	return fstest.MapFS{
		"index.html":       &fstest.MapFile{Data: []byte("<html>public</html>")},
		"admin/index.html": &fstest.MapFile{Data: []byte("<html>admin</html>")},
		"_astro/app.js":    &fstest.MapFile{Data: []byte("console.log(1)")},
	}
}

func newAuthHandler(f *fakeStore, allowed []string) *auth.Handler {
	env := func(k string) string {
		switch k {
		case "GOOGLE_CLIENT_ID":
			return "test-id"
		case "GOOGLE_CLIENT_SECRET":
			return "test-secret"
		case "GOOGLE_REDIRECT_URL":
			return "http://localhost/cb"
		case "ADMIN_EMAILS":
			return strings.Join(allowed, ",")
		}
		return ""
	}
	cfg, err := auth.Load(env)
	if err != nil {
		panic(err)
	}
	return auth.New(cfg, f)
}

func do(t *testing.T, h http.Handler, method, target, body string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---------- tests públicos ----------

func TestPublicEndpoints(t *testing.T) {
	h := New(newFakeStore(), nil, testFS()).Handler()

	rec := do(t, h, "GET", "/api/event", "", nil)
	if rec.Code != 200 {
		t.Fatalf("event: %d", rec.Code)
	}

	rec = do(t, h, "GET", "/api/gifts", "", nil)
	if rec.Code != 200 {
		t.Fatalf("gifts: %d", rec.Code)
	}
	var gifts []store.GiftPublic
	if err := json.Unmarshal(rec.Body.Bytes(), &gifts); err != nil || len(gifts) != 1 {
		t.Fatalf("gifts body: %v %s", err, rec.Body.String())
	}

	rec = do(t, h, "POST", "/api/gifts/1/claim", `{"name":"Ana","phone":"+569123"}`, nil)
	if rec.Code != 201 {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
	}
	var claim map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &claim); err != nil || claim["claim_token"] == "" {
		t.Fatalf("claim body: %v", err)
	}

	rec = do(t, h, "POST", "/api/gifts/1/claim", `{"name":"Beto","phone":"+569456"}`, nil)
	if rec.Code != 409 {
		t.Fatalf("segundo claim debería ser 409, fue %d", rec.Code)
	}

	rec = do(t, h, "DELETE", "/api/gifts/1/claim", "", map[string]string{"X-Claim-Token": claim["claim_token"]})
	if rec.Code != 204 {
		t.Fatalf("release: %d", rec.Code)
	}

	rec = do(t, h, "POST", "/api/gifts/1/claim", `{"name":"Beto","phone":"+569456"}`, nil)
	if rec.Code != 201 {
		t.Fatalf("claim tras release: %d", rec.Code)
	}

	rec = do(t, h, "POST", "/api/rsvps", `{"name":"Ana","attending":true,"guests":2}`, nil)
	if rec.Code != 201 {
		t.Fatalf("rsvp: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, "POST", "/api/rsvps", `{"name":"","attending":true}`, nil)
	if rec.Code != 400 {
		t.Fatalf("rsvp sin nombre debería ser 400, fue %d", rec.Code)
	}
}

// ---------- tests admin (autenticados) ----------

func loginAs(t *testing.T, f *fakeStore, email string) *http.Cookie {
	t.Helper()
	sid := "sess-" + email
	_ = f.CreateSession(context.Background(), sid, email, "admin", time.Now().Add(time.Hour))
	return &http.Cookie{Name: "bsh_admin", Value: sid}
}

func TestAdminGate(t *testing.T) {
	f := newFakeStore()
	ah := newAuthHandler(f, []string{"edu@x.cl"})
	h := New(f, ah, testFS()).Handler()

	if rec := do(t, h, "GET", "/api/admin/gifts", "", nil); rec.Code != 401 {
		t.Fatalf("sin sesión debería ser 401, fue %d", rec.Code)
	}
	rec := do(t, h, "GET", "/admin", "", nil)
	if rec.Code != 302 {
		t.Fatalf("/admin sin sesión debería redirigir, fue %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/auth/login" {
		t.Fatalf("Location esperado /api/auth/login, fue %q", loc)
	}
	if rec := do(t, h, "GET", "/api/gifts", "", nil); rec.Code != 200 {
		t.Fatalf("público no debería bloquearse: %d", rec.Code)
	}

	c := loginAs(t, f, "edu@x.cl")
	if rec := do(t, h, "GET", "/api/admin/gifts", "", nil, c); rec.Code != 200 {
		t.Fatalf("con sesión: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/admin", "", nil, c); rec.Code != 200 {
		t.Fatalf("/admin con sesión: %d", rec.Code)
	}
}

func TestMeAndLogout(t *testing.T) {
	f := newFakeStore()
	ah := newAuthHandler(f, []string{"edu@x.cl"})
	h := New(f, ah, testFS()).Handler()

	if rec := do(t, h, "GET", "/api/auth/me", "", nil); rec.Code != 401 {
		t.Fatalf("me sin sesión: %d", rec.Code)
	}

	c := loginAs(t, f, "edu@x.cl")
	rec := do(t, h, "GET", "/api/auth/me", "", nil, c)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "edu@x.cl") {
		t.Fatalf("me con sesión: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, "POST", "/api/auth/logout", "", nil, c)
	if rec.Code != 302 {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/auth/me", "", nil); rec.Code != 401 {
		t.Fatalf("me tras logout: %d", rec.Code)
	}
}

func TestOAuthCallback(t *testing.T) {
	f := newFakeStore()
	ah := newAuthHandler(f, []string{"edu@x.cl", "cam@x.cl"})
	h := New(f, ah, testFS()).Handler()

	ah.SetProvider(fakeProvider{email: "edu@x.cl"})

	rec := do(t, h, "GET", "/api/auth/login", "", nil)
	if rec.Code != 302 {
		t.Fatalf("login: %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("falta state")
	}
	cookies := rec.Result().Cookies()

	rec = do(t, h, "GET", "/api/auth/callback?state="+state+"&code=abc", "", nil, cookies...)
	if rec.Code != 302 {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "/admin" {
		t.Fatalf("callback Location: %q", rec.Header().Get("Location"))
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "bsh_admin" && c.Value != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no se seteó cookie de sesión")
	}

	rec = do(t, h, "GET", "/api/auth/callback?state=otro&code=abc", "", nil, cookies...)
	if rec.Code != 400 {
		t.Fatalf("callback con state malo: %d", rec.Code)
	}

	ah.SetProvider(fakeProvider{email: "intruso@x.cl"})
	rec = do(t, h, "GET", "/api/auth/login", "", nil)
	cookies = rec.Result().Cookies()
	loc, _ = url.Parse(rec.Header().Get("Location"))
	state = loc.Query().Get("state")
	rec = do(t, h, "GET", "/api/auth/callback?state="+state+"&code=abc", "", nil, cookies...)
	if rec.Code != 403 {
		t.Fatalf("intruso debería ser 403, fue %d", rec.Code)
	}
}

type fakeProvider struct{ email string }

func (p fakeProvider) Exchange(context.Context, string) (string, error) { return p.email, nil }

func TestAdminCRUD(t *testing.T) {
	f := newFakeStore()
	ah := newAuthHandler(f, []string{"edu@x.cl"})
	h := New(f, ah, testFS()).Handler()
	c := loginAs(t, f, "edu@x.cl")

	rec := do(t, h, "POST", "/api/admin/gifts", `{"title":"Cuna","note":"mini","url":"https://x.cl/cuna","position":2}`, nil, c)
	if rec.Code != 201 {
		t.Fatalf("create gift: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, "PUT", "/api/admin/gifts/1", `{"title":"Silla 2","note":"","position":1}`, nil, c)
	if rec.Code != 200 {
		t.Fatalf("update gift: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, "PUT", "/api/admin/event", `{"event_date":"2026-10-04","event_time":"16:30","place":"Parque","rsvp_deadline":"2026-09-20"}`, nil, c)
	if rec.Code != 200 {
		t.Fatalf("update event: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, "PUT", "/api/admin/event", `{"event_date":"mal","place":"x"}`, nil, c)
	if rec.Code != 400 {
		t.Fatalf("fecha inválida debería ser 400, fue %d", rec.Code)
	}

	rec = do(t, h, "DELETE", "/api/admin/gifts/99", "", nil, c)
	if rec.Code != 404 {
		t.Fatalf("delete inexistente: %d", rec.Code)
	}
}

func TestStatic(t *testing.T) {
	h := New(newFakeStore(), nil, testFS()).Handler()

	if rec := do(t, h, "GET", "/", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "public") {
		t.Fatalf("index: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/_astro/app.js", "", nil); rec.Code != 200 || rec.Header().Get("Cache-Control") == "" {
		t.Fatalf("astro asset: %d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := do(t, h, "GET", "/noexiste", "", nil); rec.Code != 404 {
		t.Fatalf("404: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/robots.txt", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Disallow: /admin") {
		t.Fatalf("robots: %d", rec.Code)
	}
}

func TestRateLimit(t *testing.T) {
	h := New(newFakeStore(), nil, testFS()).Handler()
	for i := 0; i < 10; i++ {
		if rec := do(t, h, "POST", "/api/rsvps", `{"name":"x","attending":true}`, nil); rec.Code != 201 {
			t.Fatalf("rsvp %d: %d", i, rec.Code)
		}
	}
	if rec := do(t, h, "POST", "/api/rsvps", `{"name":"x","attending":true}`, nil); rec.Code != 429 {
		t.Fatalf("onceava petición debería ser 429, fue %d", rec.Code)
	}
}
