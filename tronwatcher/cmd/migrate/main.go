// Command migrate applies or rolls back goose migrations against
// TRONWATCHER_DATABASE_URL. Mirrors depositwatcher/cmd/migrate exactly.
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 || (args[0] != "up" && args[0] != "down") {
		return fmt.Errorf("usage: migrate <up|down>")
	}
	direction := args[0]

	url := os.Getenv("TRONWATCHER_DATABASE_URL")
	if url == "" {
		return fmt.Errorf("TRONWATCHER_DATABASE_URL is not set")
	}

	db, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	if direction == "up" {
		return goose.Up(db, "migrations")
	}
	return goose.Down(db, "migrations")
}
