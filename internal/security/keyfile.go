package security

// Encryption key resolution.
//
// Every credential OpenPoet stores (SSH credentials, AI provider keys, MCP
// commands/env, tunnel keys, encrypted settings) is sealed with one AES-256
// key. LoadEncryptor picks that key in this order:
//
//  1. OPENPOET_ENCRYPT_KEY, when set. Unchanged semantics: installs that set it
//     behave exactly as before and never touch the key file.
//  2. The key file next to the database (<db dir>/openpoet.key, mode 0600).
//  3. Neither exists:
//     - the database already holds encrypted values (an install that predates
//       the key file and relied on the old implicit default): the legacy key
//       derived from hostname+user is used so those values keep decrypting.
//       When it verifiably decrypts a stored value it is persisted to the key
//       file, so a later hostname/user change cannot lock the data out. A loud
//       warning is logged because that key is predictable; rotate it with
//       `openpoet-secret-migrate rotate-key`.
//     - fresh install: a random 32-byte key is generated and written to the
//       key file.
//
// The key file holds the same kind of value as OPENPOET_ENCRYPT_KEY (it is
// hashed with SHA-256 the same way). Losing it makes every stored credential
// unrecoverable; anyone who reads it can decrypt them. Back it up together
// with the database, but keep it out of shared or published copies.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeyFileName is the name of the key file stored next to the database.
const KeyFileName = "openpoet.key"

// KeySource tells where the active encryption key came from.
type KeySource string

const (
	KeySourceEnv       KeySource = "env"       // OPENPOET_ENCRYPT_KEY
	KeySourceFile      KeySource = "file"      // existing key file
	KeySourceGenerated KeySource = "generated" // fresh install, new random key file
	KeySourceLegacy    KeySource = "legacy"    // predictable hostname+user key
)

// KeyOptions configures LoadEncryptor / ResolveKey.
type KeyOptions struct {
	// EnvKey is the value of OPENPOET_ENCRYPT_KEY ("" when unset).
	EnvKey string
	// KeyFile is the key file path, normally KeyFilePath(dbPath).
	KeyFile string
	// FindEncrypted returns one stored (ciphertext, iv) pair, or found=false
	// when the database holds no encrypted value. Only consulted when neither
	// EnvKey nor the key file is available. An error aborts resolution: a key
	// is never generated when it cannot be ruled out that data already exists.
	FindEncrypted func() (ciphertext, iv string, found bool, err error)
	// Logf receives warnings (log.Printf in production). Optional.
	Logf func(format string, args ...any)
}

// ResolvedKey is the outcome of key resolution. Key is never logged.
type ResolvedKey struct {
	Key    string
	Source KeySource
	// Persisted reports whether Key is stored in the key file (always true for
	// file/generated, true for legacy only after verification).
	Persisted bool
}

// KeyFilePath returns the key file path for the database at dbPath.
func KeyFilePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), KeyFileName)
}

// LegacyDerivedKey reproduces the implicit key used by releases that had no
// key file: predictable from the machine's hostname and $USER. It exists only
// so those installs keep decrypting their data until the key is rotated.
func LegacyDerivedKey() string {
	hostname, _ := os.Hostname()
	username := os.Getenv("USER")
	if username == "" {
		username = "openpoet"
	}
	return fmt.Sprintf("openpoet-%s-%s-default-key", hostname, username)
}

// LoadEncryptor resolves the key (see the package comment above) and returns
// an Encryptor for it together with where the key came from.
func LoadEncryptor(opts KeyOptions) (*Encryptor, KeySource, error) {
	resolved, err := ResolveKey(opts)
	if err != nil {
		return nil, "", err
	}
	encryptor, err := NewEncryptor(resolved.Key)
	if err != nil {
		return nil, "", err
	}
	return encryptor, resolved.Source, nil
}

// ResolveKey implements the key resolution order without building an
// Encryptor.
func ResolveKey(opts KeyOptions) (ResolvedKey, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opts.EnvKey != "" {
		return ResolvedKey{Key: opts.EnvKey, Source: KeySourceEnv}, nil
	}
	if opts.KeyFile == "" {
		return ResolvedKey{}, errors.New("encryption key: OPENPOET_ENCRYPT_KEY is unset and no key file path was given")
	}

	key, err := ReadKeyFile(opts.KeyFile)
	if err == nil {
		if info, statErr := os.Stat(opts.KeyFile); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			logf("[Security] WARNING: key file %s is accessible by other users (mode %#o); run: chmod 600 %s",
				opts.KeyFile, info.Mode().Perm(), opts.KeyFile)
		}
		return ResolvedKey{Key: key, Source: KeySourceFile, Persisted: true}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		// Present but unreadable or empty: never silently replace it — that
		// would orphan every credential encrypted with the real key.
		return ResolvedKey{}, err
	}

	if opts.FindEncrypted == nil {
		return ResolvedKey{}, errors.New("encryption key: cannot tell whether the database holds encrypted data")
	}
	ciphertext, iv, found, err := opts.FindEncrypted()
	if err != nil {
		return ResolvedKey{}, fmt.Errorf("encryption key: inspect database for encrypted data: %w", err)
	}

	if !found {
		generated, err := GenerateKey()
		if err != nil {
			return ResolvedKey{}, fmt.Errorf("encryption key: generate: %w", err)
		}
		stored, err := createKeyFile(opts.KeyFile, generated)
		if err != nil {
			return ResolvedKey{}, err
		}
		if stored != generated {
			// Another process created the file first; use its key.
			return ResolvedKey{Key: stored, Source: KeySourceFile, Persisted: true}, nil
		}
		logf("[Security] Generated a new random encryption key in %s (mode 0600). Back it up with the database: without it stored credentials cannot be decrypted.", opts.KeyFile)
		return ResolvedKey{Key: generated, Source: KeySourceGenerated, Persisted: true}, nil
	}

	legacy := LegacyDerivedKey()
	legacyEncryptor, err := NewEncryptor(legacy)
	if err != nil {
		return ResolvedKey{}, err
	}
	if _, decryptErr := legacyEncryptor.Decrypt(ciphertext, iv); decryptErr != nil {
		// The data was encrypted with some other key (hostname or user changed,
		// or OPENPOET_ENCRYPT_KEY used to be set). Behave like before — use the
		// legacy key — but do not freeze a key that is known to be wrong.
		logf("[Security] WARNING: OPENPOET_ENCRYPT_KEY is unset, there is no key file at %s, and the stored credentials do NOT decrypt with the legacy hostname/user key. "+
			"Falling back to that key for this run without persisting it; stored credentials will fail to decrypt. "+
			"Set OPENPOET_ENCRYPT_KEY (or write the original key to %s, mode 0600) and restart.", opts.KeyFile, opts.KeyFile)
		return ResolvedKey{Key: legacy, Source: KeySourceLegacy}, nil
	}

	stored, err := createKeyFile(opts.KeyFile, legacy)
	if err != nil {
		return ResolvedKey{}, err
	}
	if stored != legacy {
		return ResolvedKey{Key: stored, Source: KeySourceFile, Persisted: true}, nil
	}
	logf("[Security] WARNING: this database was encrypted with the legacy default key, which is derived from the hostname and user name and is PREDICTABLE: "+
		"anyone with a copy of the database who can guess them can decrypt every stored credential. "+
		"The key was saved to %s (mode 0600) so existing data keeps working. "+
		"Rotate it: stop OpenPoet, back up the database, run `openpoet-secret-migrate rotate-key --db <db> --old-key-file %s --generate-new-key-file <new file>` "+
		"(see its -h), then move the new key file over %s and start OpenPoet again.", opts.KeyFile, opts.KeyFile, opts.KeyFile)
	return ResolvedKey{Key: legacy, Source: KeySourceLegacy, Persisted: true}, nil
}

// ReadKeyFile reads a key file. Surrounding whitespace is ignored; an empty
// file is an error. A missing file returns an error matching os.ErrNotExist.
func ReadKeyFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", fmt.Errorf("encryption key: read key file %s: %w", path, err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("encryption key: key file %s is empty; restore it from a backup (deleting it makes stored credentials unrecoverable)", path)
	}
	return key, nil
}

// WriteNewKeyFile atomically creates path (mode 0600) holding key. It fails
// if path already exists, so an existing key is never overwritten.
func WriteNewKeyFile(path, key string) error {
	stored, err := createKeyFile(path, key)
	if err != nil {
		return err
	}
	if stored != key {
		return fmt.Errorf("encryption key: key file %s already exists", path)
	}
	return nil
}

// createKeyFile writes key to a temporary 0600 file in the same directory and
// hard-links it into place, so the key file is either absent or complete. If
// path already exists (a concurrent writer won), the existing key is returned.
func createKeyFile(path, key string) (string, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("encryption key: create key file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("encryption key: chmod key file: %w", err)
	}
	if _, err := tmp.WriteString(key + "\n"); err != nil {
		tmp.Close()
		return "", fmt.Errorf("encryption key: write key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("encryption key: sync key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("encryption key: close key file: %w", err)
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ReadKeyFile(path)
		}
		// Filesystems without hard links: exclusive create (not crash-atomic,
		// but still never overwrites an existing key).
		if fallbackErr := writeKeyFileExclusive(path, key); fallbackErr != nil {
			if errors.Is(fallbackErr, os.ErrExist) {
				return ReadKeyFile(path)
			}
			return "", fmt.Errorf("encryption key: install key file %s: %w", path, fallbackErr)
		}
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return key, nil
}

func writeKeyFileExclusive(path, key string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}
