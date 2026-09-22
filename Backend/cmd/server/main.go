package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"babyshower/backend/internal/api"
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

	var fingerprints []string
	if v := os.Getenv("ADMIN_CERT_FINGERPRINTS"); v != "" {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				fingerprints = append(fingerprints, f)
			}
		}
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

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.New(db, fingerprints, web.Dist()).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("escuchando en :%s", port)
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
