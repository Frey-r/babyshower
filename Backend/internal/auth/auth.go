// Package auth implementa el login del admin con Google OAuth y la
// verificación de sesión para /admin y /api/admin.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// SessionStore es el contrato para guardar y recuperar sesiones admin.
type SessionStore interface {
	CreateSession(ctx context.Context, id, email string, expires time.Time) error
	GetSession(ctx context.Context, id string) (email string, err error)
	DeleteSession(ctx context.Context, id string) error
}

// Config agrupa los parámetros de OAuth.
type Config struct {
	OAuth         *oauth2.Config
	AllowedEmails []string
	SessionDays   int
	CookieSecure  bool
}

// Load carga la configuración desde variables de entorno.
//
//	GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET -> credenciales OAuth
//	GOOGLE_REDIRECT_URL -> URL absoluta del callback
//	ADMIN_EMAILS        -> lista separada por comas de correos permitidos
//
// Devuelve (nil, nil) si OAuth no está configurado (admin abierto, sólo
// para el setup inicial).
func Load(getenv func(string) string) (*Config, error) {
	cid := getenv("GOOGLE_CLIENT_ID")
	cs := getenv("GOOGLE_CLIENT_SECRET")
	ru := strings.TrimSpace(getenv("GOOGLE_REDIRECT_URL"))
	allowed := []string{}
	for _, e := range strings.Split(getenv("ADMIN_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			allowed = append(allowed, e)
		}
	}
	if cid == "" && cs == "" && len(allowed) == 0 {
		return nil, nil
	}
	if cid == "" || cs == "" {
		return nil, errors.New("GOOGLE_CLIENT_ID y GOOGLE_CLIENT_SECRET son requeridos cuando OAuth está habilitado")
	}
	if len(allowed) == 0 {
		return nil, errors.New("ADMIN_EMAILS debe contener al menos un correo")
	}
	if ru == "" {
		ru = "http://localhost:8080/api/auth/callback"
	}
	return &Config{
		OAuth: &oauth2.Config{
			ClientID:     cid,
			ClientSecret: cs,
			RedirectURL:  ru,
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint:     google.Endpoint,
		},
		AllowedEmails: allowed,
		SessionDays:   7,
		CookieSecure:  strings.HasPrefix(ru, "https://"),
	}, nil
}

// Provider intercambia un código de OAuth por el correo del usuario.
type Provider interface {
	Exchange(ctx context.Context, code string) (email string, err error)
}

type googleProvider struct{ cfg *oauth2.Config }

func (g *googleProvider) Exchange(ctx context.Context, code string) (string, error) {
	tok, err := g.cfg.Exchange(ctx, code)
	if err != nil {
		return "", fmt.Errorf("intercambio OAuth: %w", err)
	}
	cli := g.cfg.Client(ctx, tok)
	req, err := http.NewRequestWithContext(ctx, "GET", "https://openidconnect.googleapis.com/v1/userinfo", nil)
	if err != nil {
		return "", err
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", fmt.Errorf("userinfo: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("userinfo %d: %s", resp.StatusCode, string(body))
	}
	var ui struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.Unmarshal(body, &ui); err != nil {
		return "", fmt.Errorf("userinfo JSON: %w", err)
	}
	if !ui.EmailVerified {
		return "", errors.New("el correo de Google no está verificado")
	}
	if ui.Email == "" {
		return "", errors.New("Google no devolvió correo")
	}
	return strings.ToLower(ui.Email), nil
}

// Handler agrupa los endpoints de auth y la middleware Gate.
type Handler struct {
	cfg        *Config
	store      SessionStore
	provider   Provider
	cookieName string
	stateName  string
}

func New(cfg *Config, store SessionStore) *Handler {
	return &Handler{
		cfg:        cfg,
		store:      store,
		provider:   &googleProvider{cfg.OAuth},
		cookieName: "bsh_admin",
		stateName:  "bsh_state",
	}
}

func (h *Handler) SetProvider(p Provider) { h.provider = p }

func newID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	state := newID()
	http.SetCookie(w, &http.Cookie{
		Name:     h.stateName,
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, h.cfg.OAuth.AuthCodeURL(state, oauth2.AccessTypeOnline), http.StatusFound)
}

func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sc, err := r.Cookie(h.stateName)
	if err != nil || sc.Value == "" || sc.Value != q.Get("state") {
		http.Error(w, "estado OAuth inválido", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: h.stateName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	if errStr := q.Get("error"); errStr != "" {
		http.Error(w, "Google devolvió error: "+errStr, http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "falta código", http.StatusBadRequest)
		return
	}
	email, err := h.provider.Exchange(r.Context(), code)
	if err != nil {
		http.Error(w, "login falló: "+err.Error(), http.StatusBadRequest)
		return
	}
	email = strings.ToLower(strings.TrimSpace(email))
	ok := false
	for _, e := range h.cfg.AllowedEmails {
		if e == email {
			ok = true
			break
		}
	}
	if !ok {
		http.Error(w, "acceso denegado para "+email, http.StatusForbidden)
		return
	}
	sid := newID()
	exp := time.Now().AddDate(0, 0, h.cfg.SessionDays)
	if err := h.store.CreateSession(r.Context(), sid, email, exp); err != nil {
		http.Error(w, "error interno", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookieName,
		Value:    sid,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/admin", http.StatusFound)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(h.cookieName); err == nil && c.Value != "" {
		_ = h.store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: h.cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/api/auth/login", http.StatusFound)
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	email, ok := h.currentSession(r)
	if !ok {
		http.Error(w, "no autenticado", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"email": email})
}

func (h *Handler) currentSession(r *http.Request) (string, bool) {
	c, err := r.Cookie(h.cookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	email, err := h.store.GetSession(r.Context(), c.Value)
	if err != nil {
		return "", false
	}
	return email, true
}

// Gate protege /admin* y /api/admin*. Sin sesión redirige a
// /api/auth/login (para /admin) o responde 401 (para /api/admin).
func (h *Handler) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, "/admin") && !strings.HasPrefix(p, "/api/admin") {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := h.currentSession(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/admin") {
			http.Error(w, "no autenticado", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/api/auth/login", http.StatusFound)
	})
}
