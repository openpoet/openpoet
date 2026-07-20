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

// Phase 7.3: the coordinator-side briefing skill.

const MissionCoordinatorSkillName = "mission-coordinator"

const missionCoordinatorSkillContent = `You are (or are about to become) the COORDINATOR of an OpenPoet mission. Playbook:

1. **Elect yourself**: call ` + "`openpoet_coordinator_elect`" + ` with the coordination
   group (tag id). Keep the returned fence_version — every mutation needs it.
   Renew before the TTL lapses; if a call returns coordinator_fence_stale,
   re-elect and use the new fence.
2. **Start the mission**: ` + "`openpoet_start_mission`" + ` with a goal stating what
   done looks like. One active mission per group.
3. **Decompose and spawn**: one worker session per project/front via
   ` + "`openpoet_start_worker`" + ` (pass mission_id + role; use workspace_id/isolation
   for parallel fronts in the same project; backend override for heterogeneous
   workers; remote projects work exactly like local ones). Same-project fine
   parallelism belongs to the worker's own native subagents — never spawn two
   unisolated sessions into one tree.
4. **Follow by reports, not transcripts**: wake on ` + "`openpoet_await_events`" + `
   (session.turn_completed carries report_ref), read ` + "`openpoet_get_session_report`" + `
   or ` + "`openpoet_get_mission`" + ` for the rolling roster. Drill into
   read_session_history only to debug. Steer with ` + "`openpoet_send_to_worker`" + `.
5. **QC is yours**: verify claims (build/test evidence in reports) before
   integrating; documentation lands as OpenPoet Docs linked to the mission.
6. **Close**: ` + "`openpoet_update_mission_status`" + ` completed/failed with a final
   summary report of your own (openpoet_emit_session_report, finalize:true).`

// EnsureMissionCoordinatorSkill registers the coordinator briefing skill once
// (idempotent, never overwrites a user-edited skill of the same name).
func EnsureMissionCoordinatorSkill(ctx context.Context, db *database.DB) error {
	skills, err := db.ListSkills(ctx)
	if err != nil {
		return err
	}
	for _, skill := range skills {
		if skill.Name == MissionCoordinatorSkillName {
			return nil
		}
	}
	skill := &database.Skill{Name: MissionCoordinatorSkillName, Content: missionCoordinatorSkillContent, Enabled: true}
	if err := db.CreateSkillWithVersion(ctx, skill); err != nil {
		return err
	}
	log.Printf("[configsync] registered built-in skill %q (mission coordinator briefing)", MissionCoordinatorSkillName)
	return nil
}
