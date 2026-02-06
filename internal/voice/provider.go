package voice

import (
	"context"
	"io"
	"mime/multipart"
)

// TranscriptionProvider defines the interface for audio transcription services
type TranscriptionProvider interface {
	TranscribeFile(ctx context.Context, filePath string) (*TranscriptionResult, error)
	TranscribeReader(ctx context.Context, reader io.Reader, filename string) (*TranscriptionResult, error)
	TranscribeBytes(ctx context.Context, data []byte, filename string) (*TranscriptionResult, error)
	TranscribeMultipart(ctx context.Context, file multipart.File, header *multipart.FileHeader) (*TranscriptionResult, error)
	SetLanguage(language string)
}

// ProviderType represents available transcription providers
type ProviderType string

const (
	ProviderOpenAI ProviderType = "openai"
	ProviderGroq   ProviderType = "groq"
)

// String returns the string representation of the provider type
func (p ProviderType) String() string {
	return string(p)
}
