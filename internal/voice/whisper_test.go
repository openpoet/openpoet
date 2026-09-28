package voice

import (
	"encoding/json"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func TestSpeechTextDropsSegmentsWhisperFlagsAsNonSpeech(t *testing.T) {
	var resp openai.AudioResponse
	if err := json.Unmarshal([]byte(`{
		"text": " Primeira frase. Legenda Adriana Zanotto Segunda frase.",
		"segments": [
			{"text": " Primeira frase.", "no_speech_prob": 0.01, "avg_logprob": -0.2},
			{"text": " Legenda Adriana Zanotto", "no_speech_prob": 0.92, "avg_logprob": -1.4},
			{"text": " Segunda frase.", "no_speech_prob": 0.7, "avg_logprob": -0.3}
		]}`), &resp); err != nil {
		t.Fatal(err)
	}
	text, dropped := speechText(resp)
	// High no_speech_prob alone is not enough: a confident segment is speech.
	if text != "Primeira frase. Segunda frase." || dropped != 1 {
		t.Fatalf("text=%q dropped=%d", text, dropped)
	}

	resp.Segments = resp.Segments[:1]
	resp.Text = " intact "
	if text, dropped := speechText(resp); text != " intact " || dropped != 0 {
		t.Fatalf("untouched transcript changed: %q %d", text, dropped)
	}
}

func TestWhisperLanguageCode(t *testing.T) {
	for in, want := range map[string]string{"Portuguese": "pt", "english": "en", "pt": "pt", "klingon": "", "": ""} {
		if got := whisperLanguageCode(in); got != want {
			t.Errorf("whisperLanguageCode(%q) = %q, want %q", in, got, want)
		}
	}
}
