package automation

import "openpoet/internal/application"

const taskTargetDescription = `{"type":"task","id":<task id>,"project_id":<project id>} or {"project_id":<project id>,"task_id":<task id>}; both ids are required (the payload may carry them instead)`

const tasksCreateNotes = "Creates one task and returns it (its id is the task_id). project_id comes from the target or the payload and is required; title is the only required payload field. " +
	"To run the work in a session, create the task first and pass its id as task_id to sessions.create: the session starts with the task linked and reads its title and description as the brief. " +
	"awaiting_approval becomes done at once where the project (or the global setting) auto-approves verification."

const tasksUpdateNotes = "Changes only the fields sent (a partial update) and returns the task. The target must identify BOTH the project and the task " +
	`(e.g. {"type":"task","id":123,"project_id":7}); without either one the command fails with target_invalid "project_id and task_id are required", even as a dry run. ` +
	"A task_id from another project fails with task_not_found. For a status change alone, tasks.change_status is equivalent."

const tasksListNotes = "With a project (target or payload project_id) returns that project's tasks and ignores the other filters. " +
	"Without one returns {tasks, summary} across every project the client may see, filtered by status, priority and search."

// taskPayloadSchemas publishes the request contract of the task capabilities,
// which are served by the project task registry rather than the platform one.
var taskPayloadSchemas = map[application.CapabilityName]*PlatformPayloadSchema{
	application.CapabilityTasksList: newPayloadSchema(`{} (all projects) or `+projectTargetDescription, taskListPayload{},
		`{"status":"in_progress"}`, tasksListNotes),
	application.CapabilityTasksGet: newPayloadSchema(taskTargetDescription, taskReferencePayload{}, "", ""),
	application.CapabilityTasksCreate: newPayloadSchema(projectTargetDescription, createTaskPayload{},
		`{"title":"Fix the export button","description":"The CSV export ignores the date filter.","priority":"high"}`, tasksCreateNotes),
	application.CapabilityTasksUpdate: newPayloadSchema(taskTargetDescription, updateTaskPayload{},
		`{"description":"Updated brief.","priority":"urgent"}`, tasksUpdateNotes),
	application.CapabilityTasksChangeStatus: newPayloadSchema(taskTargetDescription, changeStatusPayload{}, `{"status":"done"}`, ""),
	application.CapabilityTasksDelete:       newPayloadSchema(taskTargetDescription, taskReferencePayload{}, "", ""),
	application.CapabilityTasksDuplicate:    newPayloadSchema(taskTargetDescription, taskReferencePayload{}, "", ""),
	application.CapabilityTasksAddComment:   newPayloadSchema(taskTargetDescription, addCommentPayload{}, `{"comment":"Deployed in v1.4."}`, ""),
}
