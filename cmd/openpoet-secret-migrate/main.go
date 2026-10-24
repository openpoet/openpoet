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

const usage = `openpoet-secret-migrate — offline maintenance of OpenPoet's encrypted secrets.

Run it only while OpenPoet is STOPPED, and take a database backup first, e.g.
  sqlite3 /path/openpoet.db ".backup '/path/openpoet.db.bak'"
(also copy the key file, see below). Output contains counts only — never names,
commands, keys or ciphertext.

USAGE

  openpoet-secret-migrate --db PATH [--execute]
      Encrypt legacy plaintext MCP commands/args/env and custom tool commands
      into encrypted envelopes. Dry-run unless --execute. Uses the same key as
      the server (see "Encryption key" below).

  openpoet-secret-migrate rotate-key --db PATH [--dry-run]
        (--old-key-file FILE | env OPENPOET_OLD_ENCRYPT_KEY)
        (--new-key-file FILE | --generate-new-key-file FILE | env OPENPOET_NEW_ENCRYPT_KEY)
      Re-encrypt every stored encrypted value (SSH credentials, AI provider
      keys, paired device keys, encrypted settings, MCP/custom tool envelopes)
      from the old key to the new key in ONE transaction. If any value fails
      to decrypt with the old key nothing is changed. --dry-run only decrypts
      with the old key and counts. --generate-new-key-file creates FILE (mode
      0600, must not exist) with a new random key. Keys are taken from files
      or environment variables only, never from arguments (shell history).

ENCRYPTION KEY

  The server resolves its key in this order:
    1. OPENPOET_ENCRYPT_KEY, when set;
    2. the key file next to the database: <db dir>/openpoet.key (mode 0600);
    3. otherwise it creates that file: with a new random key on a fresh
       install, or — when the database already holds data encrypted by an
       older release without a key file — with the LEGACY key derived from
       the hostname and user name. The legacy key is predictable: anyone with
       a copy of the database who can guess them can decrypt every stored
       credential. The server logs a warning while it is in use; rotate it.

  Losing the key makes every stored credential unrecoverable; anyone who can
  read it can decrypt them. Back it up with the database, keep it private.

ROTATING THE LEGACY (OR ANY) KEY

  1. Stop OpenPoet and back up the database and <db dir>/openpoet.key.
  2. Dry run:
       openpoet-secret-migrate rotate-key --db DB --dry-run \
         --old-key-file <db dir>/openpoet.key
  3. Rotate:
       openpoet-secret-migrate rotate-key --db DB \
         --old-key-file <db dir>/openpoet.key --generate-new-key-file <db dir>/openpoet.key.new
  4. Install the new key: mv <db dir>/openpoet.key.new <db dir>/openpoet.key
     (or, if the server uses OPENPOET_ENCRYPT_KEY, set it to the new key and
     pass the old one via OPENPOET_OLD_ENCRYPT_KEY in step 2/3).
  5. Start OpenPoet. If anything fails, restore the backup and the old key.
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) > 0 && args[0] == "rotate-key" {
		return runRotateKey(args[1:], output)
	}
	flags := flag.NewFlagSet("openpoet-secret-migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dbPath := flags.String("db", "", "explicit path to the offline OpenPoet SQLite database")
	execute := flags.Bool("execute", false, "execute migration; omitted means dry-run")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := io.WriteString(output, usage)
			return writeErr
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	absolute, err := existingDBPath(*dbPath)
	if err != nil {
		return err
	}

	db, err := database.OpenExisting(absolute)
	if err != nil {
		return err
	}
	defer db.Close()

	var encryptor *security.Encryptor
	if *execute {
		// Same resolution as the server, so the envelopes decrypt at runtime.
		encryptor, _, err = security.LoadEncryptor(security.KeyOptions{
			EnvKey:  os.Getenv("OPENPOET_ENCRYPT_KEY"),
			KeyFile: security.KeyFilePath(absolute),
			FindEncrypted: func() (string, string, bool, error) {
				return db.FindEncryptedValue(context.Background())
			},
			Logf: stderrLogf,
		})
		if err != nil {
			return fmt.Errorf("failed to initialize encryption from runtime configuration: %w", err)
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

func runRotateKey(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("openpoet-secret-migrate rotate-key", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dbPath := flags.String("db", "", "explicit path to the offline OpenPoet SQLite database")
	dryRun := flags.Bool("dry-run", false, "only decrypt with the old key and count; change nothing")
	oldKeyFile := flags.String("old-key-file", "", "file holding the current key (else OPENPOET_OLD_ENCRYPT_KEY)")
	newKeyFile := flags.String("new-key-file", "", "file holding the new key (else OPENPOET_NEW_ENCRYPT_KEY)")
	generateKeyFile := flags.String("generate-new-key-file", "", "create this file (0600) with a new random key and rotate to it")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := io.WriteString(output, usage)
			return writeErr
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments (keys are read from files or environment variables only)")
	}
	absolute, err := existingDBPath(*dbPath)
	if err != nil {
		return err
	}

	oldKey, err := keyFromFileOrEnv("old", *oldKeyFile, "OPENPOET_OLD_ENCRYPT_KEY")
	if err != nil {
		return err
	}
	if oldKey == "" {
		return errors.New("old key is required: --old-key-file or OPENPOET_OLD_ENCRYPT_KEY")
	}

	newKey, err := keyFromFileOrEnv("new", *newKeyFile, "OPENPOET_NEW_ENCRYPT_KEY")
	if err != nil {
		return err
	}
	if *generateKeyFile != "" {
		if newKey != "" {
			return errors.New("give only one new key source: --new-key-file, --generate-new-key-file or OPENPOET_NEW_ENCRYPT_KEY")
		}
		if _, statErr := os.Lstat(*generateKeyFile); statErr == nil {
			return fmt.Errorf("--generate-new-key-file %s already exists; refusing to overwrite a key", *generateKeyFile)
		}
		if newKey, err = security.GenerateKey(); err != nil {
			return fmt.Errorf("generate new key: %w", err)
		}
	}
	if newKey == "" && !*dryRun {
		return errors.New("new key is required: --new-key-file, --generate-new-key-file or OPENPOET_NEW_ENCRYPT_KEY")
	}
	if newKey != "" && newKey == oldKey {
		return errors.New("old and new keys are identical")
	}

	oldEncryptor, err := security.NewEncryptor(oldKey)
	if err != nil {
		return err
	}
	var newEncryptor *security.Encryptor
	if newKey != "" {
		if newEncryptor, err = security.NewEncryptor(newKey); err != nil {
			return err
		}
	}

	db, err := database.OpenExisting(absolute)
	if err != nil {
		return err
	}
	defer db.Close()

	// The generated key is written BEFORE the transaction commits: a committed
	// rotation whose key never reached disk would lose every secret.
	wroteKeyFile := false
	if *generateKeyFile != "" && !*dryRun {
		if err := security.WriteNewKeyFile(*generateKeyFile, newKey); err != nil {
			return err
		}
		wroteKeyFile = true
	}
	report, err := database.RotateEncryptionKey(context.Background(), db, oldEncryptor.Decrypt, newEncryptor, !*dryRun)
	if err != nil {
		if wroteKeyFile {
			_ = os.Remove(*generateKeyFile)
		}
		return err
	}

	result := struct {
		database.KeyRotationReport
		NewKeyFile string `json:"new_key_file,omitempty"`
	}{KeyRotationReport: report}
	if wroteKeyFile {
		result.NewKeyFile = *generateKeyFile
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}

func existingDBPath(dbPath string) (string, error) {
	if dbPath == "" {
		return "", errors.New("--db is required")
	}
	absolute, err := filepath.Abs(dbPath)
	if err != nil {
		return "", errors.New("invalid --db path")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("--db must reference an existing regular file")
	}
	return absolute, nil
}

// keyFromFileOrEnv reads a key from path or, when path is empty, from envVar.
// Giving both is ambiguous and rejected. "" means neither was provided.
func keyFromFileOrEnv(label, path, envVar string) (string, error) {
	envValue := os.Getenv(envVar)
	if path != "" && envValue != "" {
		return "", fmt.Errorf("%s key given both as a file and via %s; use one", label, envVar)
	}
	if path == "" {
		return envValue, nil
	}
	key, err := security.ReadKeyFile(path)
	if err != nil {
		return "", fmt.Errorf("%s key file: %w", label, err)
	}
	return key, nil
}

func stderrLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
