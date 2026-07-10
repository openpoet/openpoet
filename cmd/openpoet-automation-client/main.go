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
	"strings"

	"openpoet/internal/automation"
	"openpoet/internal/database"
)

type provisionOutput struct {
	ClientID  string   `json:"client_id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	Token     string   `json:"token,omitempty"`
	TokenFile string   `json:"token_file,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("openpoet-automation-client", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "path to the OpenPoet SQLite database")
	name := flags.String("name", "", "automation client name")
	rawScopes := flags.String("scopes", "", "comma-separated automation scopes")
	tokenFile := flags.String("token-file", "", "absolute path for the one-time token (created mode 0600)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if strings.TrimSpace(*dbPath) == "" {
		return errors.New("--db is required")
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("--name is required")
	}
	scopes, scopeNames, err := parseScopes(*rawScopes)
	if err != nil {
		return err
	}

	var secretFile *os.File
	if strings.TrimSpace(*tokenFile) != "" {
		path := filepath.Clean(*tokenFile)
		if !filepath.IsAbs(path) {
			return errors.New("--token-file must be an absolute path")
		}
		secretFile, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create token file: %w", err)
		}
		*tokenFile = path
	}

	db, err := database.New(*dbPath)
	if err != nil {
		cleanupSecretFile(secretFile, *tokenFile)
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	provisioned, err := automation.ProvisionClient(context.Background(), db, *name, scopes, nil)
	if err != nil {
		cleanupSecretFile(secretFile, *tokenFile)
		return fmt.Errorf("provision automation client: %w", err)
	}
	if secretFile != nil {
		if _, err := io.WriteString(secretFile, provisioned.Token+"\n"); err != nil {
			_ = db.SetAutomationClientEnabled(context.Background(), provisioned.Client.ID, false)
			cleanupSecretFile(secretFile, *tokenFile)
			return fmt.Errorf("write token file: %w", err)
		}
		if err := secretFile.Sync(); err != nil {
			_ = db.SetAutomationClientEnabled(context.Background(), provisioned.Client.ID, false)
			cleanupSecretFile(secretFile, *tokenFile)
			return fmt.Errorf("sync token file: %w", err)
		}
		if err := secretFile.Close(); err != nil {
			_ = db.SetAutomationClientEnabled(context.Background(), provisioned.Client.ID, false)
			_ = os.Remove(*tokenFile)
			return fmt.Errorf("close token file: %w", err)
		}
		secretFile = nil
	}
	output := provisionOutput{
		ClientID: provisioned.Client.ID,
		Name:     provisioned.Client.Name,
		Scopes:   scopeNames,
	}
	if *tokenFile == "" {
		output.Token = provisioned.Token
	} else {
		output.TokenFile = *tokenFile
	}
	return json.NewEncoder(stdout).Encode(output)
}

func cleanupSecretFile(file *os.File, path string) {
	if file == nil {
		return
	}
	_ = file.Close()
	_ = os.Remove(path)
}

func parseScopes(raw string) ([]automation.Scope, []string, error) {
	parts := strings.Split(raw, ",")
	scopes := make([]automation.Scope, 0, len(parts))
	seen := make(map[automation.Scope]struct{}, len(parts))
	for _, part := range parts {
		scope := automation.Scope(strings.TrimSpace(part))
		if scope == "" {
			continue
		}
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		scopes = append(scopes, scope)
	}
	if len(scopes) == 0 {
		return nil, nil, errors.New("--scopes must contain at least one explicit scope")
	}
	if _, err := automation.NewScopeSet(scopes...); err != nil {
		return nil, nil, err
	}
	names := make([]string, len(scopes))
	for index, scope := range scopes {
		names[index] = string(scope)
	}
	return scopes, names, nil
}
