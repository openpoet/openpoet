package application

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"openpoet/internal/voice"
)

const maxVoiceAudioBytes = 32 << 20

var voiceLanguagePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})?$`)

type VoiceTranscriptionPort interface {
	TranscribeAudio(context.Context, []byte, string, string) (*voice.TranscriptionResult, error)
}

type VoiceTranscriptionService struct {
	transcriber VoiceTranscriptionPort
}

func NewVoiceTranscriptionService(transcriber VoiceTranscriptionPort) *VoiceTranscriptionService {
	return &VoiceTranscriptionService{transcriber: transcriber}
}

type TranscribeVoiceCommand struct {
	Audio         []byte
	Filename      string
	Language      string
	Authorization ActionAuthorization
}

func (s *VoiceTranscriptionService) Transcribe(ctx context.Context, command TranscribeVoiceCommand) (*voice.TranscriptionResult, error) {
	if err := requireActionActor(command.Authorization); err != nil {
		return nil, err
	}
	if len(command.Audio) == 0 || len(command.Audio) > maxVoiceAudioBytes {
		return nil, validationError("voice_audio_size_invalid", "Audio must contain between 1 byte and 32 MiB")
	}
	filename := strings.TrimSpace(command.Filename)
	if filename == "" {
		filename = "recording.webm"
	}
	if filename != filepath.Base(filename) || len(filename) > 255 || strings.IndexByte(filename, 0) >= 0 {
		return nil, validationError("voice_filename_invalid", "Audio filename must be a bounded base name")
	}
	extension := strings.ToLower(filepath.Ext(filename))
	allowedExtensions := map[string]bool{
		".webm": true, ".mp3": true, ".mp4": true, ".m4a": true,
		".wav": true, ".ogg": true, ".oga": true, ".mpeg": true,
	}
	if !allowedExtensions[extension] {
		return nil, validationError("voice_filename_invalid", "Audio filename extension is not supported")
	}
	language := strings.TrimSpace(command.Language)
	if language != "" && !voiceLanguagePattern.MatchString(language) {
		return nil, validationError("voice_language_invalid", "Voice language must be a valid language tag")
	}
	if s.transcriber == nil {
		return nil, validationError("voice_transcriber_unavailable", "Voice transcriber is unavailable")
	}
	return s.transcriber.TranscribeAudio(ctx, append([]byte(nil), command.Audio...), filename, language)
}
