// aa-import loads an explicitly exported SQLite snapshot into an empty PostgreSQL.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"aa-server/internal/cryptocompat"
	"aa-server/internal/postgres"
)

func run() error {
	input := flag.String("input", "", "path to the explicit snapshot export")
	flag.Parse()
	if *input == "" {
		return fmt.Errorf("-input is required")
	}
	file, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer file.Close()
	key, err := os.ReadFile(os.Getenv("MASTER_KEY_FILE"))
	if err != nil {
		return fmt.Errorf("MASTER_KEY_FILE must point to the preserved master.key")
	}
	cipher, err := cryptocompat.NewFernet(string(key))
	if err != nil {
		return err
	}
	if err = verifyCredentials(file, cipher); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"), 2)
	if err != nil {
		return err
	}
	defer store.Close()
	counts, err := store.ImportSnapshot(ctx, file)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"imported": counts})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
