package database

import (
	"context"
	"database/sql"
	"errors"

	"openpoet/internal/devboxpublish"
)

const devboxPublicationColumns = `app, container, project_id, session_id, container_port, host_port, mode, created_at`

// ListDevboxPublications returns every published devbox port, oldest first.
func (d *DB) ListDevboxPublications(ctx context.Context) ([]devboxpublish.Publication, error) {
	var out []devboxpublish.Publication
	err := d.SelectContext(ctx, &out, `SELECT `+devboxPublicationColumns+` FROM devbox_port_publications ORDER BY created_at, app`)
	return out, err
}

// GetDevboxPublication returns the publication of app, or nil when there is none.
func (d *DB) GetDevboxPublication(ctx context.Context, app string) (*devboxpublish.Publication, error) {
	var p devboxpublish.Publication
	err := d.GetContext(ctx, &p, `SELECT `+devboxPublicationColumns+` FROM devbox_port_publications WHERE app = ?`, app)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// InsertDevboxPublication stores a new publication.
func (d *DB) InsertDevboxPublication(ctx context.Context, p devboxpublish.Publication) error {
	_, err := d.ExecContext(ctx, `INSERT INTO devbox_port_publications (`+devboxPublicationColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.App, p.Container, p.ProjectID, p.SessionID, p.ContainerPort, p.HostPort, p.Mode, p.CreatedAt)
	return err
}

// DeleteDevboxPublication removes the publication of app (no error when absent).
func (d *DB) DeleteDevboxPublication(ctx context.Context, app string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM devbox_port_publications WHERE app = ?`, app)
	return err
}
