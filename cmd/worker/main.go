package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"listingboard/internal/store"
	"listingboard/internal/worker"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	dataDir := env("DATA_DIR", "data")
	pause, err := stepPause()
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.OpenWithRetry(dsn, 30*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("worker running, pause between steps %s", pause)
	worker.Run(ctx, st, dataDir, pause)
	log.Print("worker stopped")
}

// stepPause reads STEP_PAUSE as a Go duration such as "2s" or "500ms".
// Unset means 2s.
func stepPause() (time.Duration, error) {
	raw := os.Getenv("STEP_PAUSE")
	if raw == "" {
		return 2 * time.Second, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("STEP_PAUSE must be a duration like 2s, got %q", raw)
	}
	return d, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
