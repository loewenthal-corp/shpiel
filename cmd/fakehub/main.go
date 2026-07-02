// Command fakehub runs the in-process huggingface.co simulator as a
// standalone server, seeded with fixture models. It exists so the Tilt dev
// environment and e2e tests exercise Shpiel's pull-through path without
// touching the public internet.
//
// Not part of the shipped product; development tooling only.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/fakehub"
)

func main() {
	listen := flag.String("listen", ":8081", "address to serve on")
	flag.Parse()

	hub := fakehub.New()
	seed(hub)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("fakehub listening", "addr", *listen)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           hub.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("fakehub exited", "error", err)
		os.Exit(1)
	}
}

// seed loads deterministic fixture models. Weights are pseudo-random but
// stable so commit SHAs (and therefore cache behavior) are reproducible
// across restarts.
func seed(hub *fakehub.Hub) {
	commit := hub.AddModel("fixtures/tiny-model", map[string][]byte{
		"config.json":       []byte(`{"model_type":"tiny","hidden_size":16,"num_hidden_layers":2}`),
		"tokenizer.json":    []byte(`{"version":"1.0","truncation":null,"padding":null}`),
		"model.safetensors": weights(1 << 20), // 1 MiB
		"vae/decoder.bin":   weights(64 << 10),
	})
	fmt.Printf("seeded fixtures/tiny-model @ %s\n", commit)

	commit = hub.AddModel("fixtures/nano-model", map[string][]byte{
		"config.json":       []byte(`{"model_type":"nano"}`),
		"model.safetensors": weights(16 << 10),
	})
	fmt.Printf("seeded fixtures/nano-model @ %s\n", commit)
}

// weights generates n stable pseudo-random bytes.
func weights(n int) []byte {
	buf := make([]byte, n)
	state := uint32(0x5eed)
	for i := range buf {
		// xorshift32: cheap, deterministic, incompressible enough.
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		buf[i] = byte(state)
	}
	return buf
}
