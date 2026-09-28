package voice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

type WhisperClient struct {
	client   *openai.Client
	model    string
	language string
	provider ProviderType
}

type TranscriptionResult struct {
	Text     string  `json:"text"`
	Duration float64 `json:"duration,omitempty"`
	// Language is the ISO-639-1 code the model detected (Whisper models only);
	// clients pin it on later segments of the same recording.
	Language string `json:"language,omitempty"`
}

// NewTranscriptionProvider creates a transcription provider based on type
func NewTranscriptionProvider(providerType ProviderType, apiKey string, model string) (TranscriptionProvider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("API key is required for provider %s", providerType)
	}

	var config openai.ClientConfig

	switch providerType {
	case ProviderOpenAI:
		config = openai.DefaultConfig(apiKey)
	case ProviderGroq:
		config = openai.DefaultConfig(apiKey)
		config.BaseURL = "https://api.groq.com/openai/v1"
	default:
		return nil, fmt.Errorf("unsupported provider: %s", providerType)
	}

	client := openai.NewClientWithConfig(config)

	if model == "" {
		model = DefaultModel(providerType)
	}

	return &WhisperClient{
		client:   client,
		model:    model,
		language: "",
		provider: providerType,
	}, nil
}

func (w *WhisperClient) SetLanguage(language string) {
	w.language = language
}

// TranscribeFile transcribes an audio file
func (w *WhisperClient) TranscribeFile(ctx context.Context, filePath string) (*TranscriptionResult, error) {
	req := openai.AudioRequest{
		Model:    w.model,
		FilePath: filePath,
	}

	if w.language != "" {
		req.Language = w.language
	}
	// Whisper models return segment timestamps with verbose_json, which lets
	// us log how much of the audio the transcript actually covers. The
	// gpt-4o transcription models only support json/text.
	verbose := strings.Contains(w.model, "whisper")
	if verbose {
		req.Format = openai.AudioResponseFormatVerboseJSON
	}

	resp, err := w.client.CreateTranscription(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("transcription failed: %w", err)
	}
	if !verbose {
		return &TranscriptionResult{Text: resp.Text, Duration: resp.Duration}, nil
	}

	text, dropped := speechText(resp)
	logTranscriptionCoverage(filePath, w.provider, w.model, resp, dropped)
	return &TranscriptionResult{
		Text:     text,
		Duration: resp.Duration,
		Language: whisperLanguageCode(resp.Language),
	}, nil
}

// speechText rebuilds the transcript without the segments Whisper itself
// flags as non-speech — the same test its reference decoder applies
// (no_speech_prob > 0.6 with avg_logprob < -1). Those segments are where it
// hallucinates stock phrases ("Legenda Adriana Zanotto") over silence.
func speechText(resp openai.AudioResponse) (string, int) {
	if len(resp.Segments) == 0 {
		return resp.Text, 0
	}
	kept := make([]string, 0, len(resp.Segments))
	dropped := 0
	for _, seg := range resp.Segments {
		if seg.NoSpeechProb > 0.6 && seg.AvgLogprob < -1 {
			dropped++
			continue
		}
		kept = append(kept, strings.TrimSpace(seg.Text))
	}
	if dropped == 0 {
		return resp.Text, 0
	}
	return strings.TrimSpace(strings.Join(kept, " ")), dropped
}

// whisperLanguages maps the language names Whisper reports in verbose_json to
// ISO-639-1 codes accepted by the language request field.
var whisperLanguages = map[string]string{
	"english": "en", "portuguese": "pt", "spanish": "es", "french": "fr", "german": "de",
	"italian": "it", "dutch": "nl", "russian": "ru", "ukrainian": "uk", "polish": "pl",
	"czech": "cs", "romanian": "ro", "hungarian": "hu", "greek": "el", "turkish": "tr",
	"swedish": "sv", "danish": "da", "norwegian": "no", "finnish": "fi", "catalan": "ca",
	"galician": "gl", "basque": "eu", "arabic": "ar", "hebrew": "he", "hindi": "hi",
	"chinese": "zh", "japanese": "ja", "korean": "ko", "vietnamese": "vi", "thai": "th",
	"indonesian": "id", "malay": "ms",
}

func whisperLanguageCode(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	if code, ok := whisperLanguages[language]; ok {
		return code
	}
	if len(language) == 2 {
		return language
	}
	return ""
}

// logTranscriptionCoverage records audio duration against the span the
// returned segments cover, plus any gap over 5 s between segments, so a
// transcript that silently skipped part of a recording is visible in the logs.
func logTranscriptionCoverage(filePath string, provider ProviderType, model string, resp openai.AudioResponse, dropped int) {
	size := int64(0)
	if info, err := os.Stat(filePath); err == nil {
		size = info.Size()
	}
	lastEnd, prevEnd := 0.0, 0.0
	gaps := []string{}
	for _, seg := range resp.Segments {
		if seg.Start-prevEnd > 5 {
			gaps = append(gaps, fmt.Sprintf("%.1f-%.1fs", prevEnd, seg.Start))
		}
		prevEnd = seg.End
		if seg.End > lastEnd {
			lastEnd = seg.End
		}
	}
	log.Printf("[voice] transcribed %s/%s bytes=%d duration=%.1fs language=%q segments=%d dropped_nonspeech=%d covered_until=%.1fs chars=%d gaps=%v",
		provider, model, size, resp.Duration, resp.Language, len(resp.Segments), dropped, lastEnd, len(resp.Text), gaps)
}

// TranscribeReader transcribes audio from an io.Reader
func (w *WhisperClient) TranscribeReader(ctx context.Context, reader io.Reader, filename string) (*TranscriptionResult, error) {
	// Create a temporary file
	ext := filepath.Ext(filename)
	if ext == "" {
		ext = ".webm"
	}

	tmpFile, err := os.CreateTemp("", "whisper-*"+ext)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Copy data to temp file
	if _, err := io.Copy(tmpFile, reader); err != nil {
		return nil, fmt.Errorf("failed to write audio data: %w", err)
	}
	tmpFile.Close()

	return w.TranscribeFile(ctx, tmpFile.Name())
}

// TranscribeBytes transcribes audio from bytes
func (w *WhisperClient) TranscribeBytes(ctx context.Context, data []byte, filename string) (*TranscriptionResult, error) {
	return w.TranscribeReader(ctx, bytes.NewReader(data), filename)
}

// TranscribeMultipart handles multipart form data directly
func (w *WhisperClient) TranscribeMultipart(ctx context.Context, file multipart.File, header *multipart.FileHeader) (*TranscriptionResult, error) {
	// Determine file extension
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		// Try to determine from content type
		contentType := header.Header.Get("Content-Type")
		switch {
		case strings.Contains(contentType, "webm"):
			ext = ".webm"
		case strings.Contains(contentType, "mp3"):
			ext = ".mp3"
		case strings.Contains(contentType, "mp4"):
			ext = ".mp4"
		case strings.Contains(contentType, "wav"):
			ext = ".wav"
		case strings.Contains(contentType, "ogg"):
			ext = ".ogg"
		default:
			ext = ".webm"
		}
	}

	// Create temp file
	tmpFile, err := os.CreateTemp("", "whisper-*"+ext)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Copy uploaded file to temp file
	if _, err := io.Copy(tmpFile, file); err != nil {
		return nil, fmt.Errorf("failed to save uploaded file: %w", err)
	}
	tmpFile.Close()

	return w.TranscribeFile(ctx, tmpFile.Name())
}

// DirectAPITranscribe calls the OpenAI API directly without the SDK
// Useful for custom handling or when the SDK has issues
func DirectAPITranscribe(ctx context.Context, apiKey string, audioData []byte, filename string, model string) (*TranscriptionResult, error) {
	if model == "" {
		model = ModelGPT4oMiniTranscribe
	}
	// Create multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add file field
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	part.Write(audioData)

	// Add model field
	writer.WriteField("model", model)

	writer.Close()

	// Create request
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Send request
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var result struct {
		Text string `json:"text"`
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Simple JSON parsing
	text := string(body)
	if idx := strings.Index(text, `"text":"`); idx != -1 {
		start := idx + 8
		end := strings.Index(text[start:], `"`)
		if end != -1 {
			result.Text = text[start : start+end]
		}
	}

	return &TranscriptionResult{Text: result.Text}, nil
}
