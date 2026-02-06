package macro

import (
	"context"
	"devmanager/internal/database"
)

// BuiltinMacros returns the list of pre-built macros
var BuiltinMacros = []database.Macro{
	{
		Name:        "install-claude-code",
		Description: "Install Claude Code CLI using npm",
		Script: `#!/bin/bash
set -e
echo "Checking for Node.js..."
if ! command -v node &> /dev/null; then
    echo "Node.js is not installed. Please install Node.js first."
    exit 1
fi
echo "Node.js version: $(node --version)"

echo "Checking for npm..."
if ! command -v npm &> /dev/null; then
    echo "npm is not installed. Please install npm first."
    exit 1
fi
echo "npm version: $(npm --version)"

echo "Installing Claude Code CLI globally..."
npm install -g @anthropic-ai/claude-code

echo "Verifying installation..."
if command -v claude &> /dev/null; then
    echo "Claude Code CLI installed successfully!"
    claude --version
else
    echo "Installation may have succeeded but 'claude' command not found in PATH"
    echo "Try restarting your terminal or adding npm global bin to PATH"
fi
`,
		TargetType: "any",
		IsBuiltin:  true,
	},
	{
		Name:        "configure-auth",
		Description: "Run Claude config to set up API authentication",
		Script: `#!/bin/bash
echo "Starting Claude Code configuration..."
echo "This will open an interactive configuration."
echo ""
claude config
`,
		TargetType: "any",
		IsBuiltin:  true,
	},
	{
		Name:        "check-claude-version",
		Description: "Check the installed version of Claude Code CLI",
		Script: `#!/bin/bash
echo "Checking Claude Code CLI installation..."
echo ""
if command -v claude &> /dev/null; then
    echo "Claude Code CLI is installed"
    echo "Version: $(claude --version)"
    echo "Location: $(which claude)"
else
    echo "Claude Code CLI is NOT installed"
    echo "Run the 'install-claude-code' macro to install it"
fi
`,
		TargetType: "any",
		IsBuiltin:  true,
	},
	{
		Name:        "update-claude-code",
		Description: "Update Claude Code CLI to the latest version",
		Script: `#!/bin/bash
set -e
echo "Updating Claude Code CLI..."

if ! command -v npm &> /dev/null; then
    echo "npm is not installed. Cannot update."
    exit 1
fi

echo "Current version:"
claude --version 2>/dev/null || echo "Not currently installed"

echo ""
echo "Updating to latest version..."
npm update -g @anthropic-ai/claude-code

echo ""
echo "New version:"
claude --version
echo ""
echo "Update complete!"
`,
		TargetType: "any",
		IsBuiltin:  true,
	},
	{
		Name:        "check-project-setup",
		Description: "Check if a project has Claude Code configuration",
		Script: `#!/bin/bash
echo "Checking project Claude Code setup..."
echo ""
echo "Working directory: $(pwd)"
echo ""

if [ -d ".claude" ]; then
    echo "✓ .claude directory exists"

    if [ -f ".claude/settings.json" ]; then
        echo "✓ settings.json found"
        echo "  Contents:"
        cat .claude/settings.json | head -20
    else
        echo "✗ settings.json not found"
    fi

    if [ -f ".claude/mcp.json" ]; then
        echo "✓ mcp.json found"
    else
        echo "✗ mcp.json not found"
    fi

    if [ -d ".claude/skills" ]; then
        echo "✓ skills directory exists"
        skill_count=$(ls -1 .claude/skills/*.md 2>/dev/null | wc -l)
        echo "  Found $skill_count skill file(s)"
    else
        echo "✗ skills directory not found"
    fi
else
    echo "✗ .claude directory does not exist"
    echo "  Project has not been configured for Claude Code"
fi
`,
		TargetType: "any",
		IsBuiltin:  true,
	},
	{
		Name:        "init-claude-config",
		Description: "Initialize Claude Code configuration in project",
		Script: `#!/bin/bash
echo "Initializing Claude Code configuration..."
echo ""

mkdir -p .claude/skills

if [ ! -f ".claude/settings.json" ]; then
    echo '{}' > .claude/settings.json
    echo "✓ Created .claude/settings.json"
else
    echo "✓ .claude/settings.json already exists"
fi

if [ ! -f ".claude/mcp.json" ]; then
    echo '{"mcpServers":{}}' > .claude/mcp.json
    echo "✓ Created .claude/mcp.json"
else
    echo "✓ .claude/mcp.json already exists"
fi

echo ""
echo "Claude Code configuration initialized!"
`,
		TargetType: "any",
		IsBuiltin:  true,
	},
}

// InitBuiltinMacros inserts built-in macros into the database if they don't exist
func InitBuiltinMacros(ctx context.Context, db *database.DB) error {
	for _, macro := range BuiltinMacros {
		// Check if macro already exists
		existing, err := db.GetMacroByName(ctx, macro.Name)
		if err == nil && existing != nil {
			continue
		}

		// Create the macro
		m := macro // Copy to avoid pointer issues
		if err := db.CreateMacro(ctx, &m); err != nil {
			return err
		}
	}
	return nil
}
