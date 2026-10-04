package session

import (
	"log"
	"strings"
)

// codexTurnHookQueueSize bounds the lifecycle hooks waiting to be posted.
const codexTurnHookQueueSize = 32

// Codex app-server has no hook bridge of its own, so the runner reports turn
// boundaries to /api/hooks/event the way Claude Code's hooks do: a
// mode_changed=executing when a turn starts and a Stop (carrying the turn's
// last assistant message) when it ends. Stop closes the session's turn, idles
// its mode and becomes session.turn_completed on the event outbox.

func (r *CodexRunner) postTurnStarted(turnID string) {
	r.queueTurnHook(map[string]interface{}{
		"hook_event_name": "mode_changed",
		"mode":            "executing",
		"turn_id":         turnID,
	})
}

// postTurnStopped reports the end of a turn; status is completed, failed or
// interrupted.
func (r *CodexRunner) postTurnStopped(turnID, status string) {
	event := map[string]interface{}{
		"hook_event_name": "Stop",
		"turn_id":         turnID,
		"turn_status":     status,
	}
	if text := r.lastTurnAssistantText(); text != "" {
		event["last_assistant_message"] = text
	}
	r.queueTurnHook(event)
}

// markTurnTranscriptStart remembers where the current turn's transcript
// begins, so the Stop hook only reports this turn's reply.
func (r *CodexRunner) markTurnTranscriptStart() {
	r.mu.Lock()
	r.turnTranscriptSeq = r.transcriptSeq
	r.mu.Unlock()
}

// lastTurnAssistantText merges the chunks of the newest assistant message
// written since the turn started ("" when the turn wrote none).
func (r *CodexRunner) lastTurnAssistantText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	lastID := 0
	for _, event := range r.transcript {
		if event.Kind == "assistant" && event.ID > r.turnTranscriptSeq && event.ID > lastID {
			lastID = event.ID
		}
	}
	if lastID == 0 {
		return ""
	}
	var text strings.Builder
	for _, event := range r.transcript {
		if event.ID != lastID {
			continue
		}
		if !event.Append {
			text.Reset()
		}
		text.WriteString(event.Text)
	}
	return strings.TrimSpace(text.String())
}

// queueTurnHook posts lifecycle hooks in order on one goroutine, off the
// app-server read loop. A full queue drops the hook rather than stall Codex.
func (r *CodexRunner) queueTurnHook(event map[string]interface{}) {
	if r.cfg == nil || r.cfg.ServerAddr == "" || r.cfg.SessionID == "" {
		return
	}
	r.turnHooksOnce.Do(func() {
		r.turnHooks = make(chan map[string]interface{}, codexTurnHookQueueSize)
		go r.runTurnHooks()
	})
	select {
	case r.turnHooks <- event:
	default:
		log.Printf("[Codex] turn hook queue full for session %s; dropped %v", r.cfg.SessionID, event["hook_event_name"])
	}
}

func (r *CodexRunner) runTurnHooks() {
	post := func(event map[string]interface{}) {
		if _, err := r.postHook("event", event); err != nil {
			log.Printf("[Codex] turn hook %v for session %s failed: %v", event["hook_event_name"], r.cfg.SessionID, err)
		}
	}
	for {
		select {
		case event := <-r.turnHooks:
			post(event)
		case <-r.done:
			for {
				select {
				case event := <-r.turnHooks:
					post(event)
				default:
					return
				}
			}
		}
	}
}
