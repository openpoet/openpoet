package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"openpoet/internal/application"
	"openpoet/internal/automation"
	"openpoet/internal/voice"

	"github.com/go-chi/chi/v5"
)

type recordingVoicePort struct {
	audio    []byte
	filename string
	calls    int
	err      error
}

func (p *recordingVoicePort) TranscribeAudio(_ context.Context, audio []byte, filename, _ string) (*voice.TranscriptionResult, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	p.audio = audio
	p.filename = filename
	return &voice.TranscriptionResult{Text: "ok"}, nil
}

func voiceUploadRouter(t *testing.T, port *recordingVoicePort) (http.Handler, string) {
	t.Helper()
	registry, err := automation.NewPlatformCapabilityRegistry(application.NewCapabilityRegistry())
	if err != nil {
		t.Fatal(err)
	}
	api := &API{
		platformCapabilities: registry,
		platformServices: &PlatformApplicationServices{
			Execution: automation.ExecutionPlatformServices{Voice: application.NewVoiceTranscriptionService(port)},
		},
	}
	h := NewVoiceHandler(api, nil)
	h.uploadDir = t.TempDir()
	r := chi.NewRouter()
	r.Post("/voice/uploads/{uploadId}/chunks/{index}", h.UploadChunk)
	r.Post("/voice/uploads/{uploadId}/complete", h.CompleteUpload)
	r.Delete("/voice/uploads/{uploadId}", h.DiscardUpload)
	return r, h.uploadDir
}

func voicePostJSON(t *testing.T, router http.Handler, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestVoiceChunkedUploadReassemblesOriginalBytes(t *testing.T) {
	port := &recordingVoicePort{}
	router, root := voiceUploadRouter(t, port)
	id := "rec-0123456789abcdef"

	original := bytes.Repeat([]byte("0123456789"), 70_000) // 700 KB, 3 slices
	slices := [][]byte{original[:256<<10], original[256<<10 : 512<<10], original[512<<10:]}

	// Send out of order, skipping slice 1, and complete: must report it missing.
	for _, i := range []int{2, 0} {
		rec := voicePostJSON(t, router, "/voice/uploads/"+id+"/chunks/"+string(rune('0'+i)),
			map[string]string{"data": base64.StdEncoding.EncodeToString(slices[i])})
		if rec.Code != http.StatusOK {
			t.Fatalf("chunk %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := voicePostJSON(t, router, "/voice/uploads/"+id+"/complete", map[string]any{"chunks": 3, "filename": "recording.webm"})
	if rec.Code != http.StatusConflict || port.calls != 0 {
		t.Fatalf("incomplete upload: %d %s calls=%d", rec.Code, rec.Body.String(), port.calls)
	}
	var conflict struct{ Missing []int }
	json.Unmarshal(rec.Body.Bytes(), &conflict)
	if len(conflict.Missing) != 1 || conflict.Missing[0] != 1 {
		t.Fatalf("missing = %v", conflict.Missing)
	}

	// Raw octet-stream slice, re-sent twice (idempotent).
	for n := 0; n < 2; n++ {
		req := httptest.NewRequest(http.MethodPost, "/voice/uploads/"+id+"/chunks/1", bytes.NewReader(slices[1]))
		req.Header.Set("Content-Type", "application/octet-stream")
		raw := httptest.NewRecorder()
		router.ServeHTTP(raw, req)
		if raw.Code != http.StatusOK {
			t.Fatalf("raw chunk: %d %s", raw.Code, raw.Body.String())
		}
	}

	rec = voicePostJSON(t, router, "/voice/uploads/"+id+"/complete", map[string]any{"chunks": 3, "filename": "recording.webm"})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(port.audio, original) || port.filename != "recording.webm" {
		t.Fatalf("reassembled %d bytes, want %d (filename %q)", len(port.audio), len(original), port.filename)
	}
	if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
		t.Fatalf("staging dir not removed after success: %v", err)
	}
}

func TestVoiceChunkedUploadKeepsSlicesWhenTranscriptionFails(t *testing.T) {
	port := &recordingVoicePort{err: context.DeadlineExceeded}
	router, root := voiceUploadRouter(t, port)
	id := "rec-keep-on-failure"
	if rec := voicePostJSON(t, router, "/voice/uploads/"+id+"/chunks/0",
		map[string]string{"data": base64.StdEncoding.EncodeToString([]byte("audio"))}); rec.Code != http.StatusOK {
		t.Fatalf("chunk: %d", rec.Code)
	}
	if rec := voicePostJSON(t, router, "/voice/uploads/"+id+"/complete", map[string]any{"chunks": 1, "filename": "recording.webm"}); rec.Code == http.StatusOK {
		t.Fatal("expected failure")
	}
	if _, err := os.Stat(filepath.Join(root, id, voiceChunkName(0))); err != nil {
		t.Fatalf("slice discarded after failed transcription: %v", err)
	}
	port.err = nil
	if rec := voicePostJSON(t, router, "/voice/uploads/"+id+"/complete", map[string]any{"chunks": 1, "filename": "recording.webm"}); rec.Code != http.StatusOK {
		t.Fatalf("retry complete: %d %s", rec.Code, rec.Body.String())
	}
}

func TestVoiceChunkedUploadRejectsUnsafeInput(t *testing.T) {
	router, _ := voiceUploadRouter(t, &recordingVoicePort{})
	for _, path := range []string{"/voice/uploads/short/chunks/0", "/voice/uploads/rec-0123456789/chunks/-1", "/voice/uploads/rec-0123456789/chunks/5000"} {
		if rec := voicePostJSON(t, router, path, map[string]string{"data": "YQ=="}); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, voiceUploadMaxChunkBytes+1))
	if rec := voicePostJSON(t, router, "/voice/uploads/rec-0123456789/chunks/0", map[string]string{"data": big}); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized chunk: %d", rec.Code)
	}
}
