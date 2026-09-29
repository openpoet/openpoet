package sessionprompt

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Question sources.
const (
	SourceHook     = "hook"     // a PermissionRequest hook parked in OpenPoet
	SourceTerminal = "terminal" // a dialog painted on the session's terminal
)

// Question kinds. Clients must treat an unknown kind like KindSelection.
const (
	KindWorkspaceTrust    = "workspace_trust"    // "Do you trust the files in this folder?"
	KindBypassPermissions = "bypass_permissions" // the --dangerously-skip-permissions disclaimer
	KindToolPermission    = "tool_permission"    // an agent asks to run a tool
	KindAskUserQuestion   = "ask_user_question"  // the agent's AskUserQuestion tool
	KindPlanApproval      = "plan_approval"      // ExitPlanMode: approve or reject a plan
	KindConfirm           = "confirm"            // a typed y/n confirmation
	KindSelection         = "selection"          // any other menu
)

// Option is one answer a question accepts. Index is 1-based and is what
// clients send back.
type Option struct {
	Index       int    `json:"index"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// GrantsPermission marks answers that let the agent do something it asked
	// permission for (run a tool, bypass permissions). They require a reason.
	GrantsPermission bool `json:"grants_permission,omitempty"`
	// EndsSession marks answers after which the agent process exits.
	EndsSession bool `json:"ends_session,omitempty"`
}

// SubQuestion is one entry of a multi-question prompt (AskUserQuestion).
type SubQuestion struct {
	Header      string   `json:"header,omitempty"`
	Text        string   `json:"text"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multi_select,omitempty"`
}

// Question is the standard shape of anything a session is blocked on.
type Question struct {
	ID     string `json:"question_id"`
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	// Options is empty for free-text-only questions.
	Options []Option `json:"options,omitempty"`
	// SelectedIndex is the option the terminal cursor is on (1-based, 0 when
	// unknown). Terminal answers navigate from it.
	SelectedIndex int `json:"selected_index,omitempty"`
	// AcceptsText says the answer may carry free text: a deny reason, plan
	// feedback, an "Other" answer, or the text of a y/n prompt.
	AcceptsText bool          `json:"accepts_text,omitempty"`
	Questions   []SubQuestion `json:"questions,omitempty"`
	ToolName    string        `json:"tool_name,omitempty"`
	ToolInput   string        `json:"tool_input,omitempty"`
	DetectedAt  time.Time     `json:"detected_at"`
}

// Answer is what a client sends back. Exactly one of Option, Options, Text or
// Answers is normally set; Text may accompany Option where the question
// accepts text.
type Answer struct {
	Option  int               `json:"option,omitempty"`
	Options []int             `json:"options,omitempty"`
	Text    string            `json:"text,omitempty"`
	Answers map[string]string `json:"answers,omitempty"`
}

// OptionByIndex returns the option with the given 1-based index.
func (q *Question) OptionByIndex(index int) (Option, bool) {
	for _, option := range q.Options {
		if option.Index == index {
			return option, true
		}
	}
	return Option{}, false
}

// HashID derives a stable question id from its content, so the same dialog
// keeps its id across repaints and a different dialog never reuses it.
func HashID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(sum[:8])
}

var (
	pointerRunes   = []rune{'❯', '›', '▸', '►'}
	numberedLabel  = regexp.MustCompile(`^(\d{1,2})[.)]\s+(.+)$`)
	confirmPattern = regexp.MustCompile(`(?i)(\((?:y/n|yes/no)\)|\[(?:y/n|yes/no)\])\s*[:?]?\s*$`)
	footerPattern  = regexp.MustCompile(`(?i)enter to (?:confirm|select|submit)|esc to (?:cancel|exit|go back)|↑/↓|to navigate`)
	boxSides       = strings.NewReplacer("│", " ", "┃", " ", "║", " ", "╎", " ")
)

const (
	maxQuestionText  = 2000
	maxQuestionLines = 14
)

// Detect finds the interactive question currently painted on a terminal,
// or nil. lines is the visible screen and cursorRow the cursor line.
func Detect(lines []string, cursorRow int) *Question {
	clean := make([][]rune, len(lines))
	for i, line := range lines {
		clean[i] = []rune(strings.TrimRight(boxSides.Replace(line), " "))
	}
	for i := len(clean) - 1; i >= 0; i-- {
		if q := selectionAt(clean, i); q != nil {
			return q
		}
	}
	return confirmAt(clean, cursorRow)
}

type menuLine struct {
	row      int
	label    string
	selected bool
	option   bool // false: continuation/description of the option above
}

// selectionAt recognises a menu whose pointer ("❯ label") sits on row i.
func selectionAt(lines [][]rune, i int) *Question {
	pointerCol, labelCol := pointerColumns(lines[i])
	if pointerCol < 0 {
		return nil
	}
	block := []menuLine{{row: i, label: strings.TrimSpace(string(lines[i][labelCol:])), selected: true, option: true}}
	classify := func(row int) (menuLine, bool) {
		line := lines[row]
		first := firstNonSpace(line)
		switch {
		case first < 0:
			return menuLine{}, false
		case first == labelCol:
			return menuLine{row: row, label: strings.TrimSpace(string(line[labelCol:])), option: true}, true
		case first > labelCol:
			return menuLine{row: row, label: strings.TrimSpace(string(line[first:]))}, true
		}
		return menuLine{}, false
	}
	top := i
	for row := i - 1; row >= 0 && i-row <= 12; row-- {
		item, ok := classify(row)
		if !ok {
			break
		}
		block = append([]menuLine{item}, block...)
		top = row
	}
	// Leading continuation lines belong to the question, not to the menu.
	for len(block) > 0 && !block[0].option {
		block = block[1:]
		top++
	}
	for row := i + 1; row < len(lines) && row-i <= 12; row++ {
		item, ok := classify(row)
		if !ok {
			break
		}
		block = append(block, item)
	}
	var options []Option
	selected := 0
	for _, item := range block {
		if item.option {
			options = append(options, Option{Index: len(options) + 1, Label: item.label})
			if item.selected {
				selected = len(options)
			}
			continue
		}
		if len(options) > 0 {
			last := &options[len(options)-1]
			last.Description = strings.TrimSpace(last.Description + " " + item.label)
		}
	}
	if len(options) < 2 || len(options) > 20 {
		return nil
	}
	numbered := true
	for n, option := range options {
		match := numberedLabel.FindStringSubmatch(option.Label)
		if match == nil || match[1] != strconv.Itoa(n+1) {
			numbered = false
			break
		}
	}
	if numbered {
		for n := range options {
			options[n].Label = numberedLabel.FindStringSubmatch(options[n].Label)[2]
		}
	}
	end := block[len(block)-1].row
	footer := false
	for row := end + 1; row < len(lines) && row <= end+4; row++ {
		if footerPattern.MatchString(string(lines[row])) {
			footer = true
			break
		}
	}
	text := questionText(lines, top)
	if !numbered && !footer && !strings.HasSuffix(strings.TrimSpace(text), "?") {
		return nil
	}
	q := &Question{
		Source: SourceTerminal, Kind: KindSelection, Text: text,
		Options: options, SelectedIndex: selected,
	}
	classifyTerminalQuestion(q)
	labels := make([]string, 0, len(options))
	for _, option := range options {
		labels = append(labels, option.Label)
	}
	q.ID = HashID("t_", q.Kind, q.Text, strings.Join(labels, "\x01"))
	return q
}

func pointerColumns(line []rune) (int, int) {
	first := firstNonSpace(line)
	if first < 0 {
		return -1, -1
	}
	isPointer := false
	for _, r := range pointerRunes {
		if line[first] == r {
			isPointer = true
		}
	}
	if !isPointer {
		return -1, -1
	}
	label := -1
	for col := first + 1; col < len(line); col++ {
		if line[col] != ' ' {
			label = col
			break
		}
	}
	if label < 0 || label-first > 4 {
		return -1, -1
	}
	return first, label
}

func firstNonSpace(line []rune) int {
	for col, r := range line {
		if r != ' ' {
			return col
		}
	}
	return -1
}

func isSeparator(line []rune) bool {
	count := 0
	for _, r := range line {
		switch r {
		case ' ':
		case '─', '━', '═', '╌', '┄', '╭', '╮', '╰', '╯', '┌', '┐', '└', '┘', '-', '▔', '▁':
			count++
		default:
			return false
		}
	}
	return count >= 8
}

// questionText collects the paragraph(s) above a menu, up to a separator.
func questionText(lines [][]rune, top int) string {
	var collected []string
	blankRun := 0
	for row := top - 1; row >= 0 && top-row <= maxQuestionLines; row-- {
		line := lines[row]
		if isSeparator(line) {
			break
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			blankRun++
			if blankRun >= 3 {
				break
			}
			collected = append(collected, "")
			continue
		}
		blankRun = 0
		collected = append(collected, trimmed)
	}
	for l, r := 0, len(collected)-1; l < r; l, r = l+1, r-1 {
		collected[l], collected[r] = collected[r], collected[l]
	}
	text := strings.TrimSpace(strings.Join(collected, "\n"))
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return truncateRunes(text, maxQuestionText)
}

func classifyTerminalQuestion(q *Question) {
	lowerText := strings.ToLower(q.Text)
	hasLabel := func(fragment string) bool {
		for _, option := range q.Options {
			if strings.Contains(strings.ToLower(option.Label), fragment) {
				return true
			}
		}
		return false
	}
	switch {
	case hasLabel("i trust this folder"):
		q.Kind = KindWorkspaceTrust
	case strings.Contains(lowerText, "bypass permissions mode"):
		q.Kind = KindBypassPermissions
	case hasLabel("don't ask again") || hasLabel("tell claude what to do differently") ||
		strings.Contains(lowerText, "do you want to proceed") || strings.Contains(lowerText, "do you want to make this edit") ||
		strings.Contains(lowerText, "do you want to create") || strings.Contains(lowerText, "allow command") ||
		strings.Contains(lowerText, "would you like to run"):
		q.Kind = KindToolPermission
	}
	for n := range q.Options {
		label := strings.ToLower(q.Options[n].Label)
		switch q.Kind {
		case KindWorkspaceTrust, KindBypassPermissions:
			if strings.HasPrefix(label, "no") && strings.Contains(label, "exit") {
				q.Options[n].EndsSession = true
			}
			if q.Kind == KindBypassPermissions && strings.HasPrefix(label, "yes") {
				q.Options[n].GrantsPermission = true
			}
		case KindToolPermission:
			if strings.HasPrefix(label, "yes") || strings.HasPrefix(label, "allow") || strings.HasPrefix(label, "always") {
				q.Options[n].GrantsPermission = true
			}
			if strings.Contains(label, "what to do differently") || strings.Contains(label, "tell codex") {
				q.AcceptsText = true
			}
		}
	}
}

// confirmAt recognises a typed "(y/n)" prompt on the cursor line.
func confirmAt(lines [][]rune, cursorRow int) *Question {
	if cursorRow < 0 || cursorRow >= len(lines) {
		return nil
	}
	line := strings.TrimSpace(string(lines[cursorRow]))
	if line == "" || !confirmPattern.MatchString(line) {
		return nil
	}
	text := line
	if prior := questionText(lines, cursorRow); prior != "" {
		text = prior + "\n" + line
	}
	text = truncateRunes(text, maxQuestionText)
	q := &Question{
		Source: SourceTerminal, Kind: KindConfirm, Text: text, AcceptsText: true,
		Options: []Option{{Index: 1, Label: "y"}, {Index: 2, Label: "n"}},
	}
	q.ID = HashID("t_", q.Kind, text)
	return q
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}

// Truncate bounds a string to limit runes, marking the cut.
func Truncate(value string, limit int) string { return truncateRunes(value, limit) }

// NavigationKeys returns the key presses that move a terminal menu from the
// selected option to target (both 1-based) and confirm it.
func NavigationKeys(selected, target int) [][]byte {
	if selected <= 0 {
		selected = 1
	}
	var keys [][]byte
	for ; selected < target; selected++ {
		keys = append(keys, []byte("\x1b[B"))
	}
	for ; selected > target; selected-- {
		keys = append(keys, []byte("\x1b[A"))
	}
	return append(keys, []byte("\r"))
}
