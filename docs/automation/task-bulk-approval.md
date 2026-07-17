# Bulk task verification approval

`tasks.approve_verification_bulk` approves multiple task verifications through one command. It has the same `tasks:write` scope and `explicit` approval policy as `tasks.approve_verification`.

The payload accepts exactly one selection mode:

- `{"task_ids":[12,13,14]}` approves an explicit list.
- `{"all_pending":true}` approves every task whose stored status is `awaiting_approval`.
- `{"all_pending":true,"project_id":20}` limits that selection to one project. A command target of type `project` can provide the same scope.

The regular UI endpoint is `POST /api/tasks/approve-bulk` with the same payload shape.

## Authorization gate

Automation callers need `tasks:write` and one explicit approval grant bound to the `tasks.approve_verification_bulk` capability, command ID, client, and authorization correlation reference. The grant authorizes exactly one bulk command; it is not reused once consumed. This deliberately makes one authorization request cover the declared batch while preserving the same gate level as individual verification approval.

Every task that transitions from `awaiting_approval` to `done` receives its own `verification_approved` history entry. The entry records the actor and timestamp and includes `bulk: true`, `approval_mode: "bulk"`, and the batch identifier. A task already in `done` is returned as `already_done` and does not receive a duplicate history entry.

Failures are isolated per task. The response contains `approved`, `already_done`, and `failed` totals plus a `results` item for every selected task. A failed item does not roll back successful items.
