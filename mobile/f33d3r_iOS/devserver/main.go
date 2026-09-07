// Command devserver is the F33D3R iOS app's local backend.
//
// It serves the `/api/v1` JSON contract and the `POST /events` write lane the
// app was built against, over SQLite, on the address the app's DEBUG build
// already points at. It exists because the machine developing the app cannot
// run the platform's Docker stack, and it is a stand-in — the handlers are
// written to lift into feed-engine when that is where they run.
//
//	go run .                       # 127.0.0.1:8081, data in ./data
//	F33D3R_ADDR=0.0.0.0:8081 go run .   # reachable from a phone on the LAN
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"f33d3r.com/ios/devserver/internal/api"
	"f33d3r.com/ios/devserver/internal/store"
)

func main() {
	addr := flag.String("addr", envOr("F33D3R_ADDR", "127.0.0.1:8081"), "listen address")
	dataDir := flag.String("data", envOr("F33D3R_DATA", "data"), "directory for the database and media")
	reset := flag.Bool("reset", false, "delete the database and reseed")
	seedMediaDir := flag.String("seedmedia", envOr("F33D3R_SEED_MEDIA", "seedmedia"), "directory of the images seeded works reference")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	dbPath := filepath.Join(*dataDir, "f33d3r.sqlite")
	if *reset {
		for _, f := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
			os.Remove(f)
		}
		log.Printf("reset: removed %s", dbPath)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	seeded, err := st.SeedIfEmpty(context.Background())
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	if seeded {
		log.Printf("seeded dev accounts (password %q): @tehanibentley @admin @miiyazuko @dev @guest", store.DevPassword)
	}

	// Frequencies seed on their own: a database created before live audio
	// existed still gets a room to look at on the next start.
	freqSeeded, err := st.SeedFrequencies(context.Background())
	if err != nil {
		log.Fatalf("seed frequencies: %v", err)
	}
	if freqSeeded {
		log.Printf("seeded frequencies: one live room hosted by @miiyazuko, one scheduled by @dev")
	}

	// A KLIPY app key turns GIF search into the provider's; without one the
	// picker is served from <seedmedia>/gifs. KLIPHY_API is feed-engine's name.
	gifKey := envOr("KLIPY_API_KEY", os.Getenv("KLIPHY_API"))

	mediaDir := filepath.Join(*dataDir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		log.Fatalf("media dir: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(st, mediaDir).WithSeedMedia(*seedMediaDir).WithGifProvider(gifKey).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("F33D3R iOS dev server listening on http://%s  (db %s)", *addr, dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
