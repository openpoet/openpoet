# OpenPoet Custom Instructions

## OpenPoet CLI Tools

You have access to OpenPoet project management tools via the CLI binary at `$OPENPOET_BIN`.

### Discover available tools

```bash
$OPENPOET_BIN cli tools
```

### Call a tool

```bash
$OPENPOET_BIN cli call <tool_name> '<json_args>'
```

### Examples

```bash
$OPENPOET_BIN cli call openpoet_list_tasks '{"project_id":"1"}'
$OPENPOET_BIN cli call openpoet_create_task '{"project_id":"1","title":"Fix bug"}'
$OPENPOET_BIN cli call openpoet_update_task '{"project_id":"1","task_id":"5","status":"done"}'
$OPENPOET_BIN cli call openpoet_get_session_info '{}'
```

Always run `$OPENPOET_BIN cli tools` first to discover the exact tools available in the current session.

