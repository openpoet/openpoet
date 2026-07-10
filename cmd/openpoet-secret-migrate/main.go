package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"openpoet/internal/database"
	"openpoet/internal/security"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("openpoet-secret-migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dbPath := flags.String("db", "", "explicit path to the offline OpenPoet SQLite database")
	execute := flags.Bool("execute", false, "execute migration; omitted means dry-run")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *dbPath == "" {
		return errors.New("--db is required")
	}
	absolute, err := filepath.Abs(*dbPath)
	if err != nil {
		return errors.New("invalid --db path")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("--db must reference an existing regular file")
	}

	db, err := database.OpenExisting(absolute)
	if err != nil {
		return err
	}
	defer db.Close()

	var encryptor *security.Encryptor
	if *execute {
		encryptor, err = security.NewEncryptor(os.Getenv("OPENPOET_ENCRYPT_KEY"))
		if err != nil {
			return errors.New("failed to initialize encryption from runtime configuration")
		}
	}
	report, err := database.MigrateLegacyRuntimeSecrets(context.Background(), db, encryptor, *execute)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(struct {
		GlobalMCPRecords  int `json:"global_mcp_records"`
		ProjectMCPRecords int `json:"project_mcp_records"`
		CustomToolRecords int `json:"custom_tool_records"`
		Fields            int `json:"fields"`
	}{
		GlobalMCPRecords: report.GlobalMCPRecords, ProjectMCPRecords: report.ProjectMCPRecords,
		CustomToolRecords: report.CustomToolRecords, Fields: report.Fields,
	})
}
