// Package deployrecord reads the record .scripts/deploy.sh keeps of the last
// deploy (.run/deploy.record.json). The server reads it on boot to tell a
// deploy restart from any other restart, and publishes the outcome as
// platform.deploy.completed / platform.deploy.failed.
package deployrecord

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the record's name inside the production run directory.
const FileName = "deploy.record.json"

// States written by deploy.sh. Every state but StateRunning is final.
const (
	StateRunning        = "running"
	StateSucceeded      = "succeeded"
	StateFailed         = "failed"          // production untouched (build, pull) or interrupted
	StateRolledBack     = "rolled_back"     // new binary unhealthy, previous one serving again
	StateRollbackFailed = "rollback_failed" // new binary unhealthy and rollback failed: production down
)

// Record is one deploy. Restarts counts how many times the deploy stopped
// production: 1 for a normal deploy, 2 when it rolled back.
type Record struct {
	ID                 string     `json:"id"`
	Commit             string     `json:"commit"`
	PreviousVersion    string     `json:"previous_version"`
	Version            string     `json:"version"`
	RequestedBySession string     `json:"requested_by_session"`
	StartedAt          time.Time  `json:"started_at"`
	StoppedAt          *time.Time `json:"stopped_at,omitempty"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	Restarts           int        `json:"restarts"`
	State              string     `json:"state"`
	Step               string     `json:"step"`
	Health             string     `json:"health"`
	Rollback           string     `json:"rollback"`
	Detail             string     `json:"detail"`
}

// Final reports whether deploy.sh is done with this record.
func (r *Record) Final() bool { return r != nil && r.State != "" && r.State != StateRunning }

// Succeeded reports whether the new build is serving.
func (r *Record) Succeeded() bool { return r != nil && r.State == StateSucceeded }

// RestartKey identifies one production stop by this deploy, so each restart
// it caused is attributed once (a rollback restarts production a second time).
func (r *Record) RestartKey() string {
	if r == nil || r.Restarts <= 0 {
		return ""
	}
	return fmt.Sprintf("%s#%d", r.ID, r.Restarts)
}

// Outcome is a one-line, human description of the result (Portuguese, like
// the prompts it goes into).
func (r *Record) Outcome() string {
	if r == nil {
		return ""
	}
	commit := r.Commit
	if commit == "" {
		commit = "?"
	}
	switch r.State {
	case StateSucceeded:
		return fmt.Sprintf("deploy de %s concluído com sucesso (health check OK, versão %s)", commit, orUnknown(r.Version))
	case StateRolledBack:
		return fmt.Sprintf("deploy de %s FALHOU no health check e foi revertido para o binário anterior (versão %s)", commit, orUnknown(r.Version))
	case StateRollbackFailed:
		return fmt.Sprintf("deploy de %s FALHOU e o rollback também falhou: produção pode estar fora", commit)
	case StateFailed:
		detail := r.Detail
		if detail == "" {
			detail = r.Step
		}
		if r.Restarts == 0 {
			return fmt.Sprintf("deploy de %s FALHOU antes de parar a produção (%s); a produção seguiu no binário anterior", commit, detail)
		}
		return fmt.Sprintf("deploy de %s FALHOU (%s)", commit, detail)
	case StateRunning:
		return fmt.Sprintf("deploy de %s ainda em andamento (etapa: %s)", commit, orUnknown(r.Step))
	}
	return fmt.Sprintf("deploy de %s em estado %q", commit, r.State)
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "desconhecida"
	}
	return value
}

// Read loads the record. A missing file returns (nil, nil).
func Read(path string) (*Record, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if record.ID == "" {
		return nil, fmt.Errorf("parse %s: record has no id", path)
	}
	return &record, nil
}

// DefaultPath is OPENPOET_DEPLOY_RECORD when set, otherwise the record next
// to the running binary (.run/openpoet → .run/deploy.record.json). A server
// started from anywhere else (dev, tests) just never finds one.
func DefaultPath() string {
	if path := strings.TrimSpace(os.Getenv("OPENPOET_DEPLOY_RECORD")); path != "" {
		return path
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), FileName)
}
