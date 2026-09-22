package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrAlreadyClaimed = errors.New("regalo ya reservado")
	ErrNotFound       = errors.New("no encontrado")
)

type DB struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("crear pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping a postgres: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (db *DB) Close() { db.pool.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS gifts (
	id         SERIAL PRIMARY KEY,
	position   INT  NOT NULL DEFAULT 0,
	title      TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT '',
	url        TEXT,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS gift_claims (
	gift_id     INT PRIMARY KEY REFERENCES gifts(id) ON DELETE CASCADE,
	guest_name  TEXT NOT NULL,
	guest_phone TEXT NOT NULL,
	claim_token TEXT NOT NULL,
	claimed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rsvps (
	id         SERIAL PRIMARY KEY,
	name       TEXT NOT NULL,
	phone      TEXT,
	attending  BOOLEAN NOT NULL,
	guests     INT NOT NULL DEFAULT 0,
	message    TEXT,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS event_settings (
	id            INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
	event_date    DATE NOT NULL,
	event_time    TIME,
	place         TEXT NOT NULL DEFAULT '',
	rsvp_deadline DATE
);

CREATE TABLE IF NOT EXISTS admin_sessions (
	id         TEXT PRIMARY KEY,
	email      TEXT NOT NULL,
	role       TEXT NOT NULL DEFAULT 'admin',
	expires_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- columnas añadidas en versiones posteriores (idempotente)
ALTER TABLE admin_sessions ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'admin';

CREATE INDEX IF NOT EXISTS admin_sessions_expires_at_idx ON admin_sessions(expires_at);
`

var seedGifts = []struct {
	title, note, url string
}{
	{"Silla de auto para newborn", "Convertible 360 Isofix", "https://mamasmateas.com/products/silla-de-auto-convertible-360-isofix-saturn-gris-bebesit"},
	{"Extractor de leche doble", "Inalámbrico, Balia", "https://mamasmateas.com/products/extractor-de-leche-inalambrico"},
	{"Bolsas para leche materna", "Con boquilla y sensor de temperatura · 25 un.", "https://mamasmateas.com/products/bolsas-para-almacenar-leche-materna-con-boquilla-y-sensor-de-tempratura-25-unidades"},
	{"Esterilizador de mamaderas", "Para microondas, 4 mamaderas", "https://mamasmateas.com/products/esterilizador-microondas-para-4-mamaderas"},
	{"Luz roja", "Para las noches y la lactancia", "https://www.mercadolibre.cl/up/MLCU4382217719"},
	{"Pañales desechables", "Talla recién nacido y talla 1", ""},
	{"Pañales de tela o tuto", "Suaves, para mudas y siesta", "https://www.mercadolibre.cl/up/MLCU4488623510"},
	{"Aspirador nasal", "Balia", "https://mamasmateas.com/products/aspirador-nasal-balia"},
	{"Termómetro digital", "Para los primeros sustos", "https://mamasmateas.com/products/termometro"},
}

// Migrate crea el schema y hace seed de datos iniciales si la BD está vacía.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("migrar schema: %w", err)
	}

	var n int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM gifts`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		for i, g := range seedGifts {
			var url *string
			if g.url != "" {
				url = &g.url
			}
			_, err := db.pool.Exec(ctx,
				`INSERT INTO gifts (position, title, note, url) VALUES ($1, $2, $3, $4)`,
				i+1, g.title, g.note, url)
			if err != nil {
				return fmt.Errorf("seed gift %d: %w", i+1, err)
			}
		}
	}

	var hasEvent bool
	if err := db.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_settings WHERE id = 1)`).Scan(&hasEvent); err != nil {
		return err
	}
	if !hasEvent {
		_, err := db.pool.Exec(ctx,
			`INSERT INTO event_settings (id, event_date, event_time, place, rsvp_deadline)
			 VALUES (1, '2026-10-03', NULL, 'Av. Almirante Blanco Encalada #1723', '2026-09-26')`)
		if err != nil {
			return fmt.Errorf("seed event: %w", err)
		}
	}
	return nil
}

// ---------- tipos ----------

type Event struct {
	EventDate    string  `json:"event_date"`
	EventTime    *string `json:"event_time"`
	Place        string  `json:"place"`
	RsvpDeadline *string `json:"rsvp_deadline"`
}

type GiftPublic struct {
	ID       int     `json:"id"`
	Position int     `json:"position"`
	Title    string  `json:"title"`
	Note     string  `json:"note"`
	URL      *string `json:"url"`
	Claimed  bool    `json:"claimed"`
}

type GiftClaimInfo struct {
	GuestName  string `json:"guest_name"`
	GuestPhone string `json:"guest_phone"`
	ClaimedAt  string `json:"claimed_at"`
}

type GiftAdmin struct {
	GiftPublic
	Claim *GiftClaimInfo `json:"claim"`
}

type GiftInput struct {
	Position int     `json:"position"`
	Title    string  `json:"title"`
	Note     string  `json:"note"`
	URL      *string `json:"url"`
}

type RsvpInput struct {
	Name     string  `json:"name"`
	Phone    *string `json:"phone"`
	Attending bool   `json:"attending"`
	Guests   int     `json:"guests"`
	Message  *string `json:"message"`
}

type Rsvp struct {
	ID        int      `json:"id"`
	Name      string   `json:"name"`
	Phone     *string  `json:"phone"`
	Attending bool     `json:"attending"`
	Guests    int      `json:"guests"`
	Message   *string  `json:"message"`
	CreatedAt string   `json:"created_at"`
}

// ---------- evento ----------

func (db *DB) GetEvent(ctx context.Context) (Event, error) {
	var ev Event
	var eventTime, deadline *time.Time
	err := db.pool.QueryRow(ctx,
		`SELECT event_date::text, event_time, place, rsvp_deadline FROM event_settings WHERE id = 1`,
	).Scan(&ev.EventDate, &eventTime, &ev.Place, &deadline)
	if err != nil {
		return ev, err
	}
	if eventTime != nil {
		s := eventTime.Format("15:04")
		ev.EventTime = &s
	}
	if deadline != nil {
		s := deadline.Format("2006-01-02")
		ev.RsvpDeadline = &s
	}
	return ev, nil
}

func (db *DB) UpdateEvent(ctx context.Context, ev Event) error {
	var eventTime, deadline *string
	if ev.EventTime != nil {
		eventTime = ev.EventTime
	}
	if ev.RsvpDeadline != nil {
		deadline = ev.RsvpDeadline
	}
	_, err := db.pool.Exec(ctx,
		`UPDATE event_settings SET event_date = $1, event_time = $2::time, place = $3, rsvp_deadline = $4::date WHERE id = 1`,
		ev.EventDate, eventTime, ev.Place, deadline)
	return err
}

// ---------- regalos ----------

func (db *DB) ListGifts(ctx context.Context) ([]GiftPublic, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT g.id, g.position, g.title, g.note, g.url, (c.gift_id IS NOT NULL)
		 FROM gifts g LEFT JOIN gift_claims c ON c.gift_id = g.id
		 ORDER BY g.position, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GiftPublic
	for rows.Next() {
		var g GiftPublic
		if err := rows.Scan(&g.ID, &g.Position, &g.Title, &g.Note, &g.URL, &g.Claimed); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (db *DB) ListGiftsAdmin(ctx context.Context) ([]GiftAdmin, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT g.id, g.position, g.title, g.note, g.url, c.guest_name, c.guest_phone, c.claimed_at
		 FROM gifts g LEFT JOIN gift_claims c ON c.gift_id = g.id
		 ORDER BY g.position, g.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GiftAdmin
	for rows.Next() {
		var g GiftAdmin
		var name, phone *string
		var claimedAt *time.Time
		if err := rows.Scan(&g.ID, &g.Position, &g.Title, &g.Note, &g.URL, &name, &phone, &claimedAt); err != nil {
			return nil, err
		}
		if name != nil && phone != nil && claimedAt != nil {
			g.Claimed = true
			g.Claim = &GiftClaimInfo{
				GuestName:  *name,
				GuestPhone: *phone,
				ClaimedAt:  claimedAt.Format(time.RFC3339),
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (db *DB) CreateGift(ctx context.Context, in GiftInput) (GiftPublic, error) {
	var g GiftPublic
	err := db.pool.QueryRow(ctx,
		`INSERT INTO gifts (position, title, note, url) VALUES ($1, $2, $3, $4)
		 RETURNING id, position, title, note, url`,
		in.Position, in.Title, in.Note, in.URL).
		Scan(&g.ID, &g.Position, &g.Title, &g.Note, &g.URL)
	return g, err
}

func (db *DB) UpdateGift(ctx context.Context, id int, in GiftInput) (GiftPublic, error) {
	var g GiftPublic
	err := db.pool.QueryRow(ctx,
		`UPDATE gifts SET position = $1, title = $2, note = $3, url = $4 WHERE id = $5
		 RETURNING id, position, title, note, url`,
		in.Position, in.Title, in.Note, in.URL, id).
		Scan(&g.ID, &g.Position, &g.Title, &g.Note, &g.URL)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrNotFound
	}
	return g, err
}

func (db *DB) DeleteGift(ctx context.Context, id int) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM gifts WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) ClaimGift(ctx context.Context, giftID int, name, phone string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	var out string
	err := db.pool.QueryRow(ctx,
		`INSERT INTO gift_claims (gift_id, guest_name, guest_phone, claim_token)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (gift_id) DO NOTHING
		 RETURNING claim_token`,
		giftID, name, phone, token).Scan(&out)
	if err == nil {
		return out, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err2 := db.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM gifts WHERE id = $1)`, giftID).Scan(&exists); err2 != nil {
			return "", err2
		}
		if !exists {
			return "", ErrNotFound
		}
		return "", ErrAlreadyClaimed
	}
	return "", err
}

func (db *DB) ReleaseGift(ctx context.Context, giftID int, token string) error {
	tag, err := db.pool.Exec(ctx,
		`DELETE FROM gift_claims WHERE gift_id = $1 AND claim_token = $2`, giftID, token)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- rsvps ----------

func (db *DB) CreateRsvp(ctx context.Context, in RsvpInput) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO rsvps (name, phone, attending, guests, message) VALUES ($1, $2, $3, $4, $5)`,
		in.Name, in.Phone, in.Attending, in.Guests, in.Message)
	return err
}

func (db *DB) ListRsvps(ctx context.Context) ([]Rsvp, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id, name, phone, attending, guests, message, created_at FROM rsvps ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rsvp
	for rows.Next() {
		var r Rsvp
		var createdAt time.Time
		if err := rows.Scan(&r.ID, &r.Name, &r.Phone, &r.Attending, &r.Guests, &r.Message, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt = createdAt.Format(time.RFC3339)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- sesiones admin ----------

func (db *DB) CreateSession(ctx context.Context, id, email, role string, expires time.Time) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO admin_sessions (id, email, role, expires_at) VALUES ($1, $2, $3, $4)`,
		id, strings.ToLower(strings.TrimSpace(email)), strings.ToLower(strings.TrimSpace(role)), expires)
	return err
}

func (db *DB) GetSession(ctx context.Context, id string) (string, string, error) {
	var email, role string
	err := db.pool.QueryRow(ctx,
		`SELECT email, role FROM admin_sessions WHERE id = $1 AND expires_at > now()`, id).Scan(&email, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return strings.ToLower(email), strings.ToLower(role), err
}

func (db *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := db.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE id = $1`, id)
	return err
}

func (db *DB) PurgeExpiredSessions(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE expires_at < now()`)
	return err
}
