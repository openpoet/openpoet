package application

import (
	"context"
	"strings"
	"testing"

	"openpoet/internal/database"
)

// reportingSignals acks at once and reports the prompt the agent received,
// like the UserPromptSubmit hook does.
type reportingSignals struct {
	fakeSignals
	prompt string
	known  bool
}

func (r *reportingSignals) SubmittedPrompt(string) (string, bool) { return r.prompt, r.known }

func sendWithReportedPrompt(t *testing.T, text, received string, known bool) SendInputResult {
	t.Helper()
	store := &phase3Store{session: &database.Session{ID: "s1", Status: "running"}}
	manager := &phase3SessionManager{running: true}
	signals := &reportingSignals{fakeSignals: fakeSignals{ackCh: make(chan struct{}, 1)}, prompt: received, known: known}
	signals.ackCh <- struct{}{}
	service := NewSessionService(store, manager, nil, nil, nil, nil, nil, &phase3SessionEffects{},
		SessionCreationCollaborators{Signals: signals})
	result, err := service.SendInputWithAck(context.Background(), SendSessionInputCommand{
		SessionID: "s1", Text: text, Authorization: phase3Actor, AwaitAck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func deliveryText(chars int) string {
	var b strings.Builder
	for b.Len() < chars*2 {
		b.WriteString("Pedido: confira a configuração e indique nesta máquina, ")
	}
	// No surrounding spaces: the service trims the text before counting.
	return strings.ReplaceAll(string([]rune(b.String())[:chars]), " ", "_")
}

func TestSendInputVerifiesDeliveredPrompt(t *testing.T) {
	multiline := "primeira linha\nsegunda linha com ação\n\núltima"
	cases := []struct {
		name     string
		text     string
		received string
	}{
		{"short", "oi", "oi"},
		{"~1.5k", deliveryText(1500), deliveryText(1500)},
		{"~10k", deliveryText(10000), deliveryText(10000)},
		{"line breaks as CR", multiline, strings.ReplaceAll(multiline, "\n", "\r")},
		{"wrapped as pasted content", deliveryText(1500), "\n\n<pasted_content id=\"3a55\">\n" + deliveryText(1500) + "\n</pasted_content id=\"3a55\">\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := sendWithReportedPrompt(t, tc.text, tc.received, true)
			if !result.Acknowledged || result.Delivery != SessionDeliveryVerified {
				t.Fatalf("result = %+v, want acknowledged and verified", result)
			}
			if result.SentChars != len([]rune(tc.text)) {
				t.Fatalf("sent chars = %d, want %d", result.SentChars, len([]rune(tc.text)))
			}
		})
	}
}

// TestSendInputReportsTruncatedDelivery is the reported failure: the agent took
// only the last ~250 characters of a ~1.3k text. That is not an ack.
func TestSendInputReportsTruncatedDelivery(t *testing.T) {
	text := deliveryText(1300)
	tail := string([]rune(text)[1300-243:])
	result := sendWithReportedPrompt(t, text, tail, true)
	if result.Acknowledged || result.Delivery != SessionDeliveryMismatch {
		t.Fatalf("result = %+v, want mismatch and not acknowledged", result)
	}
	if result.SentChars != 1300 || result.ReceivedChars != 243 {
		t.Fatalf("chars sent/received = %d/%d, want 1300/243", result.SentChars, result.ReceivedChars)
	}
}

func TestSendInputUnverifiedWithoutReportedPrompt(t *testing.T) {
	result := sendWithReportedPrompt(t, "oi", "", false)
	if !result.Acknowledged || result.Delivery != SessionDeliveryUnverified {
		t.Fatalf("result = %+v, want acknowledged but unverified", result)
	}
}
