package voice

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
}

func NewWhisperClient(apiKey string) (*WhisperClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("OpenAI API key is required")
	}

	client := openai.NewClient(apiKey)
	return &WhisperClient{
		client:   client,
		model:    openai.Whisper1,
		language: "", // Auto-detect
		provider: ProviderOpenAI,
	}, nil
}

// NewTranscriptionProvider creates a transcription provider based on type
func NewTranscriptionProvider(providerType ProviderType, apiKey string) (TranscriptionProvider, error) {
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

	model := openai.Whisper1
	if providerType == ProviderGroq {
		model = "whisper-large-v3-turbo" // Groq's faster model
	}

	return &WhisperClient{
		client:   client,
		model:    model,
		language: "", // Auto-detect
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

	resp, err := w.client.CreateTranscription(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("transcription failed: %w", err)
	}

	return &TranscriptionResult{
		Text:     resp.Text,
		Duration: float64(resp.Duration),
	}, nil
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
func DirectAPITranscribe(ctx context.Context, apiKey string, audioData []byte, filename string) (*TranscriptionResult, error) {
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
	writer.WriteField("model", "whisper-1")

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
