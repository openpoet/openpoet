package database

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"openpoet/internal/secretvalue"
)

// Inventory of every place that stores values encrypted with the process
// encryption key. Two storage shapes exist:
//
//   - raw AES-GCM ciphertext with its IV in a sibling column
//     (encryptedPairColumns), and in the settings table as "<key>" plus
//     "<key>_iv" rows;
//   - secretvalue JSON envelopes ({"version","ciphertext","iv"}) in legacy text
//     columns (envelopeColumns). Envelopes are additionally swept from every
//     other table so a newly added envelope column is never left behind by a
//     key rotation.
//
// TestEncryptedColumnInventoryCoversSchema fails when a migration adds an *_iv
// column that is not listed here.
var encryptedPairColumns = []struct{ table, ciphertext, iv string }{
	{"projects", "ssh_credential_encrypted", "ssh_credential_iv"},
	{"ai_configs", "api_key_encrypted", "api_key_iv"},
	{"paired_devices", "encryption_key", "encryption_key_iv"},
}

var envelopeColumns = []struct{ table, column string }{
	{"mcp_servers", "command"},
	{"mcp_servers", "args"},
	{"mcp_servers", "env"},
	{"mcp_servers", "command_windows"},
	{"mcp_servers", "args_windows"},
	{"mcp_servers", "env_windows"},
	{"project_mcp_servers", "command"},
	{"project_mcp_servers", "args"},
	{"project_mcp_servers", "env"},
	{"project_tools", "command"},
}

type encryptedItemKind uint8

const (
	encryptedPair encryptedItemKind = iota
	encryptedSetting
	encryptedEnvelope
)

// encryptedItem is one stored encrypted value. location identifies it in
// errors without revealing any secret (table/column/rowid or setting key).
type encryptedItem struct {
	kind       encryptedItemKind
	location   string
	table      string
	column     string
	ivColumn   string
	rowID      int64
	settingKey string
	ciphertext string
	iv         string
	envelope   string
	malformed  bool
}

// FindEncryptedValue returns one stored (ciphertext, iv) pair, or found=false
// when the database holds no encrypted value at all. The key resolver uses it
// to tell an existing install (keep its key) from a fresh one (generate a new
// key), so it errs on the side of finding data.
func (d *DB) FindEncryptedValue(ctx context.Context) (ciphertext, iv string, found bool, err error) {
	stop := errors.New("found")
	var first *encryptedItem
	err = walkEncryptedItems(ctx, d.DB, func(item encryptedItem) error {
		if item.malformed {
			return nil
		}
		first = &item
		return stop
	})
	if err != nil && !errors.Is(err, stop) {
		return "", "", false, err
	}
	if first == nil {
		return "", "", false, nil
	}
	if first.kind == encryptedEnvelope {
		ciphertext, iv, _ = secretvalue.Parts(first.envelope)
		return ciphertext, iv, true, nil
	}
	return first.ciphertext, first.iv, true, nil
}

// KeyRotationReport contains counts only, safe to print from offline tooling.
type KeyRotationReport struct {
	Executed      bool `json:"executed"`
	ColumnValues  int  `json:"column_values"`
	SettingValues int  `json:"setting_values"`
	Envelopes     int  `json:"envelopes"`
	Total         int  `json:"total"`
}

// RotateEncryptionKey re-encrypts every stored encrypted value: each one is
// decrypted with decrypt (the old key) and sealed again with encryptor (the
// new key), all inside ONE transaction. It refuses — changing nothing — when
// any value fails to decrypt with the old key. execute=false only decrypts and
// counts (encryptor may then be nil).
func RotateEncryptionKey(
	ctx context.Context,
	db *DB,
	decrypt secretvalue.DecryptFunc,
	encryptor secretvalue.Encryptor,
	execute bool,
) (KeyRotationReport, error) {
	report := KeyRotationReport{Executed: execute}
	if db == nil {
		return report, fmt.Errorf("key rotation database is required")
	}
	if decrypt == nil {
		return report, fmt.Errorf("key rotation old key is required")
	}
	if execute && encryptor == nil {
		return report, fmt.Errorf("key rotation new key is required")
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("begin key rotation: %w", err)
	}
	defer tx.Rollback()

	var items []encryptedItem
	if err = walkEncryptedItems(ctx, tx, func(item encryptedItem) error {
		items = append(items, item)
		return nil
	}); err != nil {
		return report, err
	}

	plaintexts := make([]string, len(items))
	var failed []string
	for i, item := range items {
		var plaintext string
		var decryptErr error
		switch {
		case item.malformed:
			decryptErr = fmt.Errorf("malformed")
		case item.kind == encryptedEnvelope:
			plaintext, decryptErr = secretvalue.Resolve(item.envelope, decrypt)
		default:
			plaintext, decryptErr = decrypt(item.ciphertext, item.iv)
		}
		if decryptErr != nil {
			failed = append(failed, item.location)
			continue
		}
		plaintexts[i] = plaintext
		switch item.kind {
		case encryptedPair:
			report.ColumnValues++
		case encryptedSetting:
			report.SettingValues++
		case encryptedEnvelope:
			report.Envelopes++
		}
		report.Total++
	}
	if len(failed) > 0 {
		shown := failed
		if len(shown) > 20 {
			shown = shown[:20]
		}
		return report, fmt.Errorf("%d encrypted value(s) do not decrypt with the old key (wrong old key, or corrupt values); nothing was changed: %s",
			len(failed), strings.Join(shown, ", "))
	}
	if !execute {
		return report, nil
	}

	for i, item := range items {
		switch item.kind {
		case encryptedEnvelope:
			sealed, encErr := secretvalue.Encrypt(encryptor, plaintexts[i])
			if encErr != nil {
				return report, fmt.Errorf("re-encrypt %s: %w", item.location, encErr)
			}
			_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s=? WHERE rowid=?`, quoteIdent(item.table), quoteIdent(item.column)), sealed, item.rowID)
		default:
			ciphertext, iv, encErr := encryptor.Encrypt(plaintexts[i])
			if encErr != nil {
				return report, fmt.Errorf("re-encrypt %s: %w", item.location, encErr)
			}
			if item.kind == encryptedSetting {
				if _, err = tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, ciphertext, item.settingKey); err == nil {
					_, err = tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, iv, item.settingKey+"_iv")
				}
			} else {
				_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s=?, %s=? WHERE rowid=?`,
					quoteIdent(item.table), quoteIdent(item.column), quoteIdent(item.ivColumn)), ciphertext, iv, item.rowID)
			}
		}
		if err != nil {
			return report, fmt.Errorf("update %s: %w", item.location, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return report, fmt.Errorf("commit key rotation: %w", err)
	}
	return report, nil
}

type encryptedItemQueryer interface {
	sqlx.QueryerContext
}

// walkEncryptedItems visits every encrypted value in the database. Returning
// an error from visit stops the walk and is passed through.
func walkEncryptedItems(ctx context.Context, q encryptedItemQueryer, visit func(encryptedItem) error) error {
	tables, err := listTables(ctx, q)
	if err != nil {
		return err
	}
	columnNames := make(map[string][]string, len(tables))
	columnsByTable := make(map[string]map[string]bool, len(tables))
	for _, table := range tables {
		names, err := tableColumns(ctx, q, table)
		if err != nil {
			return err
		}
		columnNames[table] = names
		columnsByTable[table] = make(map[string]bool, len(names))
		for _, name := range names {
			columnsByTable[table][name] = true
		}
	}

	// 1. Raw ciphertext + IV column pairs.
	for _, pair := range encryptedPairColumns {
		columns := columnsByTable[pair.table]
		if !columns[pair.ciphertext] || !columns[pair.iv] {
			continue // older schema
		}
		rows, err := q.QueryxContext(ctx, fmt.Sprintf(
			`SELECT rowid, COALESCE(%[1]s, ''), COALESCE(%[2]s, '') FROM %[3]s
			 WHERE COALESCE(%[1]s, '') != '' OR COALESCE(%[2]s, '') != '' ORDER BY rowid`,
			quoteIdent(pair.ciphertext), quoteIdent(pair.iv), quoteIdent(pair.table)))
		if err != nil {
			return fmt.Errorf("scan %s encrypted values: %w", pair.table, err)
		}
		var batch []encryptedItem
		for rows.Next() {
			item := encryptedItem{kind: encryptedPair, table: pair.table, column: pair.ciphertext, ivColumn: pair.iv}
			if err := rows.Scan(&item.rowID, &item.ciphertext, &item.iv); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s encrypted values: %w", pair.table, err)
			}
			item.location = fmt.Sprintf("%s.%s rowid=%d", pair.table, pair.ciphertext, item.rowID)
			// Ciphertext without IV (or the reverse) can never decrypt.
			item.malformed = item.ciphertext == "" || item.iv == ""
			batch = append(batch, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("scan %s encrypted values: %w", pair.table, err)
		}
		for _, item := range batch {
			if err := visit(item); err != nil {
				return err
			}
		}
	}

	// 2. Settings stored as "<key>" + "<key>_iv". A value without an IV row is
	// legacy plaintext (migrated lazily by the runtime), not ciphertext.
	if columnsByTable["settings"]["key"] {
		var settings []struct {
			Key        string `db:"key"`
			Ciphertext string `db:"ciphertext"`
			IV         string `db:"iv"`
		}
		if err := sqlx.SelectContext(ctx, q, &settings,
			`SELECT s.key AS key, s.value AS ciphertext, i.value AS iv
			   FROM settings i JOIN settings s ON s.key = substr(i.key, 1, length(i.key) - 3)
			  WHERE i.key LIKE '%\_iv' ESCAPE '\' AND i.value != '' AND s.value != ''
			  ORDER BY s.key`); err != nil {
			return fmt.Errorf("scan encrypted settings: %w", err)
		}
		for _, setting := range settings {
			if err := visit(encryptedItem{
				kind: encryptedSetting, location: "settings[" + setting.Key + "]",
				settingKey: setting.Key, ciphertext: setting.Ciphertext, iv: setting.IV,
			}); err != nil {
				return err
			}
		}
	}

	// 3. Envelopes. Known envelope columns are strict (a malformed envelope is
	// reported); every other text column is swept for valid envelopes.
	known := make(map[string]bool, len(envelopeColumns))
	for _, column := range envelopeColumns {
		known[column.table+"."+column.column] = true
	}
	for _, table := range tables {
		for _, column := range columnNames[table] {
			strict := known[table+"."+column]
			rows, err := q.QueryxContext(ctx, fmt.Sprintf(
				`SELECT rowid, %[1]s FROM %[2]s
				 WHERE typeof(%[1]s) = 'text' AND (instr(%[1]s, '"ciphertext"') > 0 OR instr(%[1]s, '"iv"') > 0)
				 ORDER BY rowid`, quoteIdent(column), quoteIdent(table)))
			if err != nil {
				return fmt.Errorf("scan %s.%s for encrypted envelopes: %w", table, column, err)
			}
			var batch []encryptedItem
			for rows.Next() {
				item := encryptedItem{kind: encryptedEnvelope, table: table, column: column}
				if err := rows.Scan(&item.rowID, &item.envelope); err != nil {
					rows.Close()
					return fmt.Errorf("scan %s.%s for encrypted envelopes: %w", table, column, err)
				}
				if _, _, ok := secretvalue.Parts(item.envelope); !ok {
					if !strict || !secretvalue.LooksLikeEnvelope(item.envelope) {
						continue
					}
					item.malformed = true
				}
				item.location = fmt.Sprintf("%s.%s rowid=%d", table, column, item.rowID)
				batch = append(batch, item)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return fmt.Errorf("scan %s.%s for encrypted envelopes: %w", table, column, err)
			}
			for _, item := range batch {
				if err := visit(item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func listTables(ctx context.Context, q encryptedItemQueryer) ([]string, error) {
	var tables []string
	if err := sqlx.SelectContext(ctx, q, &tables,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return tables, nil
}

func tableColumns(ctx context.Context, q encryptedItemQueryer, table string) ([]string, error) {
	var names []string
	if err := sqlx.SelectContext(ctx, q, &names, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table); err != nil {
		return nil, fmt.Errorf("list columns of %s: %w", table, err)
	}
	return names, nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
