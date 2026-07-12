package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CreateSkillWithVersion persists the aggregate and its initial snapshot in a
// single transaction so observers never see a skill without version history.
func (d *DB) CreateSkillWithVersion(ctx context.Context, skill *Skill) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO skills (name, content, enabled, category, sort_order) VALUES (?, ?, ?, ?, ?)`, skill.Name, skill.Content, skill.Enabled, skill.Category, skill.SortOrder)
	if err != nil {
		return err
	}
	skill.ID, err = result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO skill_versions (skill_id, name, content, version) VALUES (?, ?, ?, 1)`, skill.ID, skill.Name, skill.Content); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	fresh, refreshErr := d.GetSkill(ctx, skill.ID)
	if refreshErr == nil {
		*skill = *fresh
	}
	return nil
}

// UpdateSkillWithVersion updates a skill and optionally creates a snapshot in
// the same transaction.
func (d *DB) UpdateSkillWithVersion(ctx context.Context, skill *Skill, snapshot bool) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE skills SET name=?, content=?, enabled=?, category=?, sort_order=?, updated_at=? WHERE id=?`, skill.Name, skill.Content, skill.Enabled, skill.Category, skill.SortOrder, time.Now(), skill.ID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	if snapshot {
		var next int
		if err = tx.GetContext(ctx, &next, `SELECT COALESCE(MAX(version), 0) + 1 FROM skill_versions WHERE skill_id = ?`, skill.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO skill_versions (skill_id, name, content, version) VALUES (?, ?, ?, ?)`, skill.ID, skill.Name, skill.Content, next); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	fresh, refreshErr := d.GetSkill(ctx, skill.ID)
	if refreshErr == nil {
		*skill = *fresh
	}
	return nil
}

func (d *DB) RestoreSkillVersionAtomic(ctx context.Context, skillID, versionID int64) (*Skill, error) {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var version SkillVersion
	if err = tx.GetContext(ctx, &version, `SELECT * FROM skill_versions WHERE id = ? AND skill_id = ?`, versionID, skillID); err != nil {
		return nil, err
	}
	var skill Skill
	if err = tx.GetContext(ctx, &skill, `SELECT * FROM skills WHERE id = ?`, skillID); err != nil {
		return nil, err
	}
	skill.Name, skill.Content = version.Name, version.Content
	if _, err = tx.ExecContext(ctx, `UPDATE skills SET name=?, content=?, updated_at=? WHERE id=?`, skill.Name, skill.Content, time.Now(), skill.ID); err != nil {
		return nil, err
	}
	var next int
	if err = tx.GetContext(ctx, &next, `SELECT COALESCE(MAX(version), 0) + 1 FROM skill_versions WHERE skill_id = ?`, skill.ID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO skill_versions (skill_id, name, content, version) VALUES (?, ?, ?, ?)`, skill.ID, skill.Name, skill.Content, next); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	fresh, refreshErr := d.GetSkill(ctx, skillID)
	if refreshErr == nil {
		return fresh, nil
	}
	return &skill, nil
}

func (d *DB) SetSettingsAtomic(ctx context.Context, values map[string]string, deleteKeys []string) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	for _, key := range deleteKeys {
		if _, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) SetAIConfigAssignmentsAtomic(ctx context.Context, assignments map[string]*int64) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for slot, configID := range assignments {
		var result sql.Result
		if configID == nil {
			result, err = tx.ExecContext(ctx, `UPDATE ai_config_assignments SET config_id = NULL WHERE slot = ?`, slot)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE ai_config_assignments SET config_id = ? WHERE slot = ?`, *configID, slot)
		}
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return fmt.Errorf("unknown AI config assignment slot %q", slot)
		}
	}
	return tx.Commit()
}

func (d *DB) ReplaceProjectSkillConfigsAtomic(ctx context.Context, projectID int64, configs map[int64]bool) error {
	var policy string
	if err := d.GetContext(ctx, &policy, `SELECT skill_policy FROM projects WHERE id = ?`, projectID); err != nil {
		return err
	}
	return d.SetProjectSkillConfigAtomic(ctx, projectID, policy, configs)
}

// SetProjectSkillConfigAtomic updates both the project policy and its global
// skill overrides as one aggregate. A failure in either half rolls everything
// back, so UI and Automation callers can never observe a mixed configuration.
func (d *DB) SetProjectSkillConfigAtomic(ctx context.Context, projectID int64, policy string, configs map[int64]bool) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE projects SET skill_policy = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, policy, projectID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM project_skill_config WHERE project_id = ?`, projectID); err != nil {
		return err
	}
	for skillID, enabled := range configs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_skill_config (project_id, skill_id, enabled) VALUES (?, ?, ?)`, projectID, skillID, enabled); err != nil {
			return err
		}
	}
	return tx.Commit()
}
