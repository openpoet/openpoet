package configsync

import (
	"context"
	"log"

	"openpoet/internal/database"
)

// Phase 7.2 (Maestro): the worker-side milestone-report protocol ships as a
// skill so every harness materializes it (.claude/skills/session-report/,
// .opencode/skills/, copilot/acp instruction bundles) and the model can invoke
// it as /session-report.

const SessionReportSkillName = "session-report"

const sessionReportSkillContent = `Emit a DENSE structured milestone report so the coordinator can follow your work
without reading your transcript. Do this at every meaningful milestone (a step
finished, a verification ran, you got blocked) and before ending your turn when
you are part of an orchestration mission.

Call the ` + "`openpoet_emit_session_report`" + ` tool with:

- ` + "`turn_id`" + `: a stable milestone id (m1, m2, ...). Re-emitting the same id UPDATES it.
- ` + "`objective`" + `: what this milestone set out to do (one line).
- ` + "`summary`" + `: tight summary of what actually happened — no transcript prose.
- ` + "`decisions`" + `: key decisions made, one line each.
- ` + "`files`" + `: files you changed.
- ` + "`verification`" + `: {status: passed|failed|partial|not_run, summary: "go test ok"}.
- ` + "`blockers`" + `: what is blocking you (marks the report incomplete).
- ` + "`needs_from_coordinator`" + `: what you need decided/unblocked by the coordinator.
- ` + "`next`" + `: the next step you will take.
- ` + "`finalize`" + `: true on the LAST report of the milestone/mission leg.

Keep it dense: the report is the coordinator's primary channel. The transcript
is only for drill-down debugging.`

// EnsureSessionReportSkill registers the global session-report skill once.
// Idempotent and non-destructive: if a skill with this name exists (possibly
// edited by the user), it is left untouched.
func EnsureSessionReportSkill(ctx context.Context, db *database.DB) error {
	skills, err := db.ListSkills(ctx)
	if err != nil {
		return err
	}
	for _, skill := range skills {
		if skill.Name == SessionReportSkillName {
			return nil
		}
	}
	skill := &database.Skill{Name: SessionReportSkillName, Content: sessionReportSkillContent, Enabled: true}
	if err := db.CreateSkillWithVersion(ctx, skill); err != nil {
		return err
	}
	log.Printf("[configsync] registered built-in skill %q (worker milestone reports)", SessionReportSkillName)
	return nil
}
