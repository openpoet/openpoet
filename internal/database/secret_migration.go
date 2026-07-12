package database

import (
	"context"
	"fmt"

	"openpoet/internal/secretvalue"
)

// LegacyRuntimeSecretMigrationReport contains counts only. It is safe to print
// from offline tooling and deliberately carries no names, commands, env, or
// ciphertext.
type LegacyRuntimeSecretMigrationReport struct {
	Executed          bool `json:"executed"`
	GlobalMCPRecords  int  `json:"global_mcp_records"`
	ProjectMCPRecords int  `json:"project_mcp_records"`
	CustomToolRecords int  `json:"custom_tool_records"`
	Fields            int  `json:"fields"`
}

// MigrateLegacyRuntimeSecrets is an explicit offline preflight/cutover step.
// execute=false performs a transactionally consistent dry run. execute=true
// encrypts every legacy plaintext field in one transaction; any malformed
// envelope or encryption failure rolls the complete migration back.
func MigrateLegacyRuntimeSecrets(
	ctx context.Context,
	db *DB,
	encryptor secretvalue.Encryptor,
	execute bool,
) (LegacyRuntimeSecretMigrationReport, error) {
	report := LegacyRuntimeSecretMigrationReport{Executed: execute}
	if db == nil {
		return report, fmt.Errorf("secret migration database is required")
	}
	if execute && encryptor == nil {
		return report, fmt.Errorf("secret migration encryptor is required")
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("begin secret migration: %w", err)
	}
	defer tx.Rollback()

	type mcpRow struct {
		ID      int64  `db:"id"`
		Command string `db:"command"`
		Args    string `db:"args"`
		Env     string `db:"env"`
	}
	var global []mcpRow
	if err = tx.SelectContext(ctx, &global, `SELECT id, command, args, env FROM mcp_servers ORDER BY id`); err != nil {
		return report, fmt.Errorf("scan global MCP secrets: %w", err)
	}
	for _, row := range global {
		command, commandChanged, resolveErr := migrateLegacySecretField(row.Command, encryptor, execute)
		if resolveErr != nil {
			return report, fmt.Errorf("global MCP record %d command: %w", row.ID, resolveErr)
		}
		args, argsChanged, resolveErr := migrateLegacySecretField(row.Args, encryptor, execute)
		if resolveErr != nil {
			return report, fmt.Errorf("global MCP record %d args: %w", row.ID, resolveErr)
		}
		env, envChanged, resolveErr := migrateLegacySecretField(row.Env, encryptor, execute)
		if resolveErr != nil {
			return report, fmt.Errorf("global MCP record %d environment: %w", row.ID, resolveErr)
		}
		changed := countChanged(commandChanged, argsChanged, envChanged)
		if changed == 0 {
			continue
		}
		report.GlobalMCPRecords++
		report.Fields += changed
		if execute {
			if _, err = tx.ExecContext(ctx, `UPDATE mcp_servers SET command=?, args=?, env=? WHERE id=?`, command, args, env, row.ID); err != nil {
				return report, fmt.Errorf("update global MCP record %d: %w", row.ID, err)
			}
		}
	}

	var project []mcpRow
	if err = tx.SelectContext(ctx, &project, `SELECT id, command, args, env FROM project_mcp_servers ORDER BY id`); err != nil {
		return report, fmt.Errorf("scan project MCP secrets: %w", err)
	}
	for _, row := range project {
		command, commandChanged, resolveErr := migrateLegacySecretField(row.Command, encryptor, execute)
		if resolveErr != nil {
			return report, fmt.Errorf("project MCP record %d command: %w", row.ID, resolveErr)
		}
		args, argsChanged, resolveErr := migrateLegacySecretField(row.Args, encryptor, execute)
		if resolveErr != nil {
			return report, fmt.Errorf("project MCP record %d args: %w", row.ID, resolveErr)
		}
		env, envChanged, resolveErr := migrateLegacySecretField(row.Env, encryptor, execute)
		if resolveErr != nil {
			return report, fmt.Errorf("project MCP record %d environment: %w", row.ID, resolveErr)
		}
		changed := countChanged(commandChanged, argsChanged, envChanged)
		if changed == 0 {
			continue
		}
		report.ProjectMCPRecords++
		report.Fields += changed
		if execute {
			if _, err = tx.ExecContext(ctx, `UPDATE project_mcp_servers SET command=?, args=?, env=? WHERE id=?`, command, args, env, row.ID); err != nil {
				return report, fmt.Errorf("update project MCP record %d: %w", row.ID, err)
			}
		}
	}

	type toolRow struct {
		ID      int64  `db:"id"`
		Command string `db:"command"`
	}
	var tools []toolRow
	if err = tx.SelectContext(ctx, &tools, `SELECT id, command FROM project_tools ORDER BY id`); err != nil {
		return report, fmt.Errorf("scan custom tool secrets: %w", err)
	}
	for _, row := range tools {
		command, changed, resolveErr := migrateLegacySecretField(row.Command, encryptor, execute)
		if resolveErr != nil {
			return report, fmt.Errorf("custom tool record %d command: %w", row.ID, resolveErr)
		}
		if !changed {
			continue
		}
		report.CustomToolRecords++
		report.Fields++
		if execute {
			if _, err = tx.ExecContext(ctx, `UPDATE project_tools SET command=? WHERE id=?`, command, row.ID); err != nil {
				return report, fmt.Errorf("update custom tool record %d: %w", row.ID, err)
			}
		}
	}

	if !execute {
		return report, nil
	}
	if err = tx.Commit(); err != nil {
		return report, fmt.Errorf("commit secret migration: %w", err)
	}
	return report, nil
}

func migrateLegacySecretField(value string, encryptor secretvalue.Encryptor, execute bool) (string, bool, error) {
	needsEncryption, err := secretvalue.NeedsEncryption(value)
	if err != nil || !needsEncryption {
		return value, false, err
	}
	if !execute {
		return value, true, nil
	}
	encrypted, err := secretvalue.Encrypt(encryptor, value)
	return encrypted, true, err
}

func countChanged(values ...bool) int {
	count := 0
	for _, changed := range values {
		if changed {
			count++
		}
	}
	return count
}
