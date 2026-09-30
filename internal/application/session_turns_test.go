package application

import (
	"context"
	"testing"
	"time"

	"openpoet/internal/database"
)

// turnSignals adds a known turn state to fakeSignals.
type turnSignals struct {
	fakeSignals
	turn  SessionTurnState
	known bool
}

func (s *turnSignals) SessionTurnState(string) (SessionTurnState, bool) { return s.turn, s.known }

func turnService(signals SessionSignalPort) (*SessionService, *phase3SessionManager) {
	store := &phase3Store{session: &database.Session{ID: "s1", Status: "running"}}
	manager := &phase3SessionManager{running: true}
	return NewSessionService(store, manager, nil, nil, nil, nil, nil, &phase3SessionEffects{},
		SessionCreationCollaborators{Signals: signals}), manager
}

// In d1f920bb the inactivity timer marked the session idle 15 s into a long
// Bash run and the guard let text through mid-turn. An open turn is busy no
// matter what the mode says, and a closed one is not.
func TestSendInputBusyMeansOpenTurnNotMode(t *testing.T) {
	signals := &turnSignals{fakeSignals: fakeSignals{mode: "idle"}, known: true,
		turn: SessionTurnState{Open: true, Since: time.Now().Add(-time.Minute), Reason: "UserPromptSubmit"}}
	service, manager := turnService(signals)
	_, err := service.SendInputWithAck(context.Background(), SendSessionInputCommand{
		SessionID: "s1", Text: "hi", Authorization: phase3Actor, RejectIfBusy: true,
	})
	if code := sendErrCode(err); code != "session_busy" || len(manager.writes) != 0 {
		t.Fatalf("open turn with idle mode: err=%v writes=%d, want session_busy and nothing typed", err, len(manager.writes))
	}

	signals.turn = SessionTurnState{Open: false, Since: time.Now(), Reason: "Stop"}
	signals.mode = "executing"
	if _, err := service.SendInputWithAck(context.Background(), SendSessionInputCommand{
		SessionID: "s1", Text: "hi", Authorization: phase3Actor, RejectIfBusy: true,
	}); err != nil {
		t.Fatalf("closed turn refused: %v", err)
	}
}

// Without any turn signal (backends without prompt hooks) the mode still guards.
func TestSendInputBusyFallsBackToModeWithoutTurnSignal(t *testing.T) {
	service, _ := turnService(&turnSignals{fakeSignals: fakeSignals{mode: "executing"}})
	_, err := service.SendInputWithAck(context.Background(), SendSessionInputCommand{
		SessionID: "s1", Text: "hi", Authorization: phase3Actor, RejectIfBusy: true,
	})
	if code := sendErrCode(err); code != "session_busy" {
		t.Fatalf("err=%v, want session_busy from the executing mode", err)
	}
}

// The turn only opens once the agent accepts the prompt, so a second guarded
// send racing the first must be refused rather than typed into it.
func TestSendInputRefusesConcurrentGuardedSend(t *testing.T) {
	signals := &turnSignals{fakeSignals: fakeSignals{mode: "idle", ackCh: make(chan struct{})}, known: true}
	service, _ := turnService(signals)
	first := make(chan error, 1)
	go func() {
		_, err := service.SendInputWithAck(context.Background(), SendSessionInputCommand{
			SessionID: "s1", Text: "first", Authorization: phase3Actor, RejectIfBusy: true, AwaitAck: true,
		})
		first <- err
	}()
	time.Sleep(100 * time.Millisecond) // the first send is now waiting for its ack
	_, err := service.SendInputWithAck(context.Background(), SendSessionInputCommand{
		SessionID: "s1", Text: "second", Authorization: phase3Actor, RejectIfBusy: true,
	})
	if code := sendErrCode(err); code != "session_busy" {
		t.Fatalf("concurrent send err=%v, want session_busy", err)
	}
	signals.ackCh <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("first send: %v", err)
	}
}

// A caller that gives up must not shorten the ack wait: the result has to say
// what the agent did.
func TestSendInputAckWaitOutlivesCallerCancellation(t *testing.T) {
	signals := &turnSignals{fakeSignals: fakeSignals{mode: "idle", ackCh: make(chan struct{}, 1)}}
	service, _ := turnService(signals)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	time.AfterFunc(150*time.Millisecond, func() { signals.ackCh <- struct{}{} })
	result, err := service.SendInputWithAck(ctx, SendSessionInputCommand{
		SessionID: "s1", Text: "hi", Authorization: phase3Actor, AwaitAck: true,
	})
	if err != nil || !result.Acknowledged {
		t.Fatalf("result=%+v err=%v, want the late ack recorded", result, err)
	}
}
