package main

import (
	"log"
	"net/http"
	"os"

	"slack-agent/backend/internal/app"
)

func main() {
	command := ""
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	cfg := app.LoadConfig()
	store, err := app.OpenStore(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	if err := store.Migrate(); err != nil {
		log.Fatal(err)
	}
	if command == "repair-analysis" {
		result, err := store.RepairAnalysisStructuredOutputs()
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Repaired %d analysis runs and %d job titles; skipped %d rows", result.AnalysisRuns, result.Jobs, result.Skipped)
		return
	}
	if command != "" {
		log.Fatalf("unknown command %q", command)
	}
	if cfg.SeedDemoData {
		if err := store.SeedDemoData(); err != nil {
			log.Fatal(err)
		}
	}

	server := app.NewServer(cfg, store)
	log.Printf("Local Slack Agent backend listening on %s", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, server.Routes()); err != nil {
		log.Fatal(err)
	}
}
