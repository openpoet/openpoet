package benchmark

import "devmanager/internal/llm"

// AllScenarios returns the complete set of benchmark scenarios.
func AllScenarios() []Scenario {
	return []Scenario{
		// ──── Tasks ────
		{
			ID:          "task-create",
			Name:        "Create task from description",
			Category:    CategoryTasks,
			Description: "Model should create a task with appropriate title and description",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "Create a task to add JWT authentication to the API endpoints"),
			},
			Validators: []ValidatorFunc{
				ValidateToolCalled("create_task"),
				ValidateMaxSentences(3),
			},
			JudgeRubric: `The assistant should:
1. Call create_task with project_id "1"
2. Title should mention JWT authentication
3. Description should be outcome-based (not implementation details)
4. Response should be brief (1-2 sentences), saying the proposal was created
5. Should NOT announce plans or summarize what it will do before acting`,
			ExpectedTools: []string{"create_task"},
		},
		{
			ID:          "task-prioritize",
			Name:        "Identify urgent tasks",
			Category:    CategoryTasks,
			Description: "Model should list tasks and identify priorities",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "Which tasks are most urgent in the test-webapp project?"),
			},
			Validators: []ValidatorFunc{
				ValidateToolCalled("list_tasks"),
				ValidateMaxSentences(4),
			},
			JudgeRubric: `The assistant should:
1. Call list_tasks with project_id "1" to get current tasks
2. Identify the urgent task (ID 4: "Fix email validation")
3. May also mention high-priority tasks (IDs 1 and 3)
4. Response should be concise (1-3 sentences)
5. Should NOT paste the full task list — just summarize priorities`,
			ExpectedTools: []string{"list_tasks"},
		},
		{
			ID:          "task-subtasks",
			Name:        "Create subtasks for umbrella task",
			Category:    CategoryTasks,
			Description: "Model should create umbrella + subtasks using parent_ref",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "Split the project deploy into subtasks: configure Docker, configure CI/CD, and configure monitoring"),
			},
			Validators: []ValidatorFunc{
				ValidateToolCalled("create_task"),
				ValidateToolCalledAtLeast("create_task", 3),
			},
			JudgeRubric: `The assistant should:
1. Create an umbrella/parent task first for the deploy setup
2. Create 3 subtasks (Docker, CI/CD, monitoring) using parent_ref="1" to reference the umbrella
3. Each subtask should have its own acceptance criteria
4. Should NOT split by technical layer — each subtask is an independent feature
5. Response should be brief, confirming the proposals were created`,
			ExpectedTools: []string{"create_task"},
		},

		// ──── Skills ────
		{
			ID:          "skill-generate",
			Name:        "Generate a Go best practices skill",
			Category:    CategorySkills,
			Description: "Model should create a skill with markdown instructions",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "Create a Go best practices skill for the project"),
			},
			Validators: []ValidatorFunc{
				ValidateToolCalled("create_skill"),
			},
			JudgeRubric: `The assistant should:
1. Call create_skill with a lowercase-hyphenated name (e.g. "go-best-practices")
2. Content should be markdown with actionable Go instructions (not vague)
3. Should include specific practices (error handling, naming, testing, etc.)
4. Category should be "coding"
5. Response should be brief (1-2 sentences)`,
			ExpectedTools: []string{"create_skill"},
		},
		{
			ID:          "skill-explain",
			Name:        "Explain what skills are",
			Category:    CategorySkills,
			Description: "Model should explain skills without calling any tool",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "What are skills in DevManager?"),
			},
			Validators: []ValidatorFunc{
				ValidateNoToolCalled(),
				ValidateMaxSentences(4),
			},
			JudgeRubric: `The assistant should:
1. NOT call any tool — this is a knowledge question answered from the system prompt
2. Explain that skills are markdown documents with instructions for Claude Code
3. Should NOT say skills are scripts, bash files, or executables
4. Response must be brief (1-3 sentences)
5. Should mention they are synced to projects for Claude Code sessions`,
			ExpectedTools: []string{},
		},

		// ──── Code Understanding ────
		{
			ID:          "code-find-handler",
			Name:        "Find health check handler",
			Category:    CategoryCode,
			Description: "Model should use tools to find a specific handler",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "Where is the health check handler in the test-webapp project?"),
			},
			Validators: []ValidatorFunc{
				ValidateAnyToolCalled("read_file", "grep_content", "find_files", "list_directory"),
			},
			JudgeRubric: `The assistant should:
1. Use at least one code exploration tool (read_file, grep_content, find_files, or list_directory)
2. Identify that the health check is in handlers/health.go
3. Response should briefly indicate the location
4. Should NOT paste the entire file content in chat
5. May optionally describe what the handler does`,
			ExpectedTools: []string{"read_file", "grep_content", "find_files", "list_directory"},
		},
		{
			ID:          "code-architecture",
			Name:        "Explain project architecture",
			Category:    CategoryCode,
			Description: "Model should use create_document for long explanations",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "Explain the complete architecture of the test-webapp project"),
			},
			Validators: []ValidatorFunc{
				ValidateToolCalled("create_document"),
			},
			JudgeRubric: `The assistant should:
1. First investigate the project (list_directory, read_file, etc.)
2. Use create_document for the explanation (since >5 lines)
3. The document should cover: main.go entry point, handlers, models, routes
4. Chat response should be very brief (1 sentence) — the document has the details
5. Should NOT paste a long explanation directly in chat`,
			ExpectedTools: []string{"create_document"},
		},

		// ──── Tool Selection ────
		{
			ID:          "tool-correct",
			Name:        "List project files",
			Category:    CategoryTools,
			Description: "Model should call list_directory for file listing",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "List the files of the test-webapp project"),
			},
			Validators: []ValidatorFunc{
				ValidateToolCalled("list_directory"),
			},
			JudgeRubric: `The assistant should:
1. Call list_directory with project_id "1"
2. Should NOT call read_file, find_files, or grep_content for this request
3. Response should be brief, showing or summarizing the file listing
4. May use create_document if the listing is long`,
			ExpectedTools: []string{"list_directory"},
		},
		{
			ID:          "tool-no-call",
			Name:        "No tool needed for gratitude",
			Category:    CategoryTools,
			Description: "Model should respond without calling any tool",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "Thanks for the help!"),
			},
			Validators: []ValidatorFunc{
				ValidateNoToolCalled(),
				ValidateMaxSentences(2),
			},
			JudgeRubric: `The assistant should:
1. NOT call any tool — this is just a social message
2. Respond briefly and naturally (e.g. "You're welcome!" or similar)
3. Should be 1 sentence maximum
4. Should NOT offer to do more or list capabilities`,
			ExpectedTools: []string{},
		},

		// ──── Memory Doc ────
		{
			ID:          "memory-view",
			Name:        "View memory doc without pasting content",
			Category:    CategoryMemory,
			Description: "Model should call get_memory_doc and respond with 1 sentence only",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "Show me the memory doc for the test-webapp project"),
			},
			Validators: []ValidatorFunc{
				ValidateToolCalled("get_memory_doc"),
				ValidateMaxSentences(2),
				ValidateNotContains("## Overview"),
				ValidateNotContains("Tech Stack"),
			},
			JudgeRubric: `The assistant should:
1. Call get_memory_doc with project_id "1"
2. Respond with ONLY 1 sentence like "Project test-webapp memory doc loaded."
3. MUST NOT paste, quote, or summarize the content from <internal_reference>
4. MUST NOT show the document content in the chat
5. The system shows a native viewer card — assistant should not generate links`,
			ExpectedTools: []string{"get_memory_doc"},
		},

		// ──── Conversational Quality ────
		{
			ID:          "conv-brevity",
			Name:        "Brevity compliance",
			Category:    CategoryConversation,
			Description: "Model should answer in 1-2 sentences per the cardinal rule",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "How many projects do we have configured?"),
			},
			Validators: []ValidatorFunc{
				ValidateToolCalled("list_projects"),
				ValidateMaxSentences(2),
			},
			JudgeRubric: `The assistant should:
1. Call list_projects to get the count
2. Answer in 1-2 sentences (cardinal brevity rule)
3. Should say something like "There is 1 project configured: test-webapp."
4. MUST NOT list project details, paths, or configuration
5. MUST NOT announce what it's about to do before calling the tool`,
			ExpectedTools: []string{"list_projects"},
		},
		{
			ID:          "conv-language",
			Name:        "Language detection (English)",
			Category:    CategoryConversation,
			Description: "Model should respond in English when user writes in English",
			Messages: []llm.Message{
				llm.NewTextMessage("user", "How many projects do we have?"),
			},
			Validators: []ValidatorFunc{
				ValidateToolCalled("list_projects"),
				ValidateResponseLanguage("en"),
				ValidateMaxSentences(2),
			},
			JudgeRubric: `The assistant should:
1. Call list_projects
2. Respond in ENGLISH (user wrote in English)
3. Should be 1-2 sentences maximum
4. The system prompt says to always respond in English.
5. Response should be in English`,
			ExpectedTools: []string{"list_projects"},
		},
	}
}
