package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"babyshower/backend/internal/api"
	"babyshower/backend/internal/auth"
	"babyshower/backend/internal/store"
	"babyshower/backend/internal/web"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	port := envOr("PORT", "8080")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL es requerido")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("conectar a la base de datos: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("migrar: %v", err)
	}
	if err := db.PurgeExpiredSessions(ctx); err != nil {
		log.Printf("purgar sesiones: %v", err)
	}

	authCfg, err := auth.Load(os.Getenv)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	var authn *auth.Handler
	if authCfg == nil {
		log.Println("ADVERTENCIA: Google OAuth no configurado; /admin queda accesible sin login. Configura GOOGLE_CLIENT_ID/SECRET y ADMIN_EMAILS antes de ir a producción.")
	} else {
		authn = auth.New(authCfg, db)
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.New(db, authn, web.Dist()).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if authCfg == nil {
			log.Printf("escuchando en :%s (admin sin protección)", port)
		} else {
			log.Printf("escuchando en :%s (oauth con %d usuario(s))", port, len(authCfg.Users))
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("servir: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("apagando...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
