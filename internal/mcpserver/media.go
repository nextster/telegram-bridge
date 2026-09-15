package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nextster/telegram-bridge/internal/media"
)

type attachmentInput struct {
	Chat      string `json:"chat" jsonschema:"Exact chat key returned by Telegram Bridge"`
	MessageID int    `json:"message_id" jsonschema:"Exact message ID returned by Telegram Bridge"`
}
type transcribeInput struct {
	Chat        string   `json:"chat" jsonschema:"Exact chat key returned by Telegram Bridge"`
	MessageID   int      `json:"message_id" jsonschema:"Exact voice or video-note message ID"`
	ConfirmPaid bool     `json:"confirm_paid" jsonschema:"Must be true only after the user explicitly requested paid cloud processing of this attachment"`
	Model       string   `json:"model,omitempty" jsonschema:"Optional model; currently openai/gpt-transcribe via OpenRouter"`
	Keywords    []string `json:"keywords,omitempty" jsonschema:"Up to 16 spelling hints, 80 bytes each and 512 bytes total; no instructions or newlines"`
	Languages   []string `json:"languages,omitempty" jsonschema:"Up to 4 expected language codes; hints do not replace detected language metadata"`
}
type imageInput struct {
	Chat                string `json:"chat" jsonschema:"Exact chat key returned by Telegram Bridge"`
	MessageID           int    `json:"message_id" jsonschema:"Exact photo or image-document message ID"`
	ConfirmPaid         bool   `json:"confirm_paid" jsonschema:"Must be true only after the user explicitly requested paid cloud processing of this image"`
	Model               string `json:"model,omitempty" jsonschema:"Optional model; currently openai/gpt-4.1-mini via OpenRouter"`
	DescriptionLanguage string `json:"description_language,omitempty" jsonschema:"ISO language code or source (visible text language, English if unknown); omission preserves legacy prompt/cache. Changing this creates a distinct paid job"`
}
type mediaResultInput struct {
	Chat      string `json:"chat" jsonschema:"Original chat key"`
	MessageID int    `json:"message_id" jsonschema:"Original message ID"`
	JobID     string `json:"job_id" jsonschema:"Job ID returned by the explicit processing tool"`
}

type mediaBatchInput struct {
	Items       []media.Reference `json:"items" jsonschema:"1..100 exact chat/message references; duplicates reuse a single job"`
	ConfirmPaid bool              `json:"confirm_paid" jsonschema:"Must be true after explicit authorization for all supplied media"`
	Audio       media.Options     `json:"audio,omitempty" jsonschema:"Optional speech model, keywords, and languages"`
	Image       media.Options     `json:"image,omitempty" jsonschema:"Optional image model and description_language; no speech hints"`
	WaitSeconds *int              `json:"wait_seconds,omitempty" jsonschema:"0..480 seconds to wait, default 480; zero queues without waiting"`
}

type mediaBatchResultInput struct {
	Items       []media.JobReference `json:"items" jsonschema:"1..100 original chat/message/job references from the batch response"`
	WaitSeconds *int                 `json:"wait_seconds,omitempty" jsonschema:"0..480 seconds to wait for existing jobs; default zero"`
}

func (s *Server) addMediaTools(server *mcp.Server) {
	read := &mcp.ToolAnnotations{ReadOnlyHint: true}
	no := false
	write := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, IdempotentHint: true}
	mcp.AddTool(server, &mcp.Tool{Name: "telegram_get_attachment", Description: "Get attachment metadata by chat and message ID. Does not download or send media to an AI provider.", Annotations: read}, s.getAttachment)
	mcp.AddTool(server, &mcp.Tool{Name: "telegram_download_attachment", Description: "Download the original voice, video note, or image to a private cloud cache. Returns an expiring URL that requires the caller's own bearer token. Does not call an AI provider.", Annotations: write}, s.downloadAttachment)
	mcp.AddTool(server, &mcp.Tool{Name: "telegram_transcribe_media", Description: "Explicit PAID action: queue verbatim speech transcription through OpenRouter. Requires confirm_paid=true. Reuses existing jobs/results. Poll telegram_get_transcription; never automatically call this while reading history.", Annotations: write}, s.transcribeMedia)
	mcp.AddTool(server, &mcp.Tool{Name: "telegram_get_transcription", Description: "Read a transcription job status and full result, with language, duration and original message metadata. No paid calls.", Annotations: read}, s.getTranscription)
	mcp.AddTool(server, &mcp.Tool{Name: "telegram_analyze_image", Description: "Explicit PAID action: analyze an image through OpenRouter. Description and verbatim OCR are stored separately from one response. Requires confirm_paid=true. Reuses cached results.", Annotations: write}, s.analyzeImage)
	mcp.AddTool(server, &mcp.Tool{Name: "telegram_get_image_analysis", Description: "Read image analysis status, scene description and separately stored OCR text. No paid calls.", Annotations: read}, s.getImageAnalysis)
	mcp.AddTool(server, &mcp.Tool{Name: "telegram_process_media_batch", Description: "Explicit PAID action: queue 1..100 voice/video-note/image references and wait for all jobs. Shared cached jobs prevent duplicate charges across concurrent batches and history calls. Returns per-item errors and pending jobs on timeout.", Annotations: write}, s.processMediaBatch)
	mcp.AddTool(server, &mcp.Tool{Name: "telegram_get_media_batch", Description: "Read or wait for existing media jobs together, without starting paid work. Preserve original chat/message/job references; pending jobs continue independently of this call.", Annotations: read}, s.getMediaBatch)
}

func (s *Server) processMediaBatch(ctx context.Context, req *mcp.CallToolRequest, in mediaBatchInput) (*mcp.CallToolResult, media.Batch, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, media.Batch{}, err
	}
	wait, err := waitDuration(in.WaitSeconds, media.MaxWait)
	if err != nil {
		return nil, media.Batch{}, err
	}
	batch, err := s.media.StartBatch(ctx, userID, in.Items, media.BatchOptions{Audio: in.Audio, Image: in.Image}, in.ConfirmPaid)
	if err == nil {
		batch, err = s.media.WaitBatch(ctx, userID, batch, wait)
	}
	return nil, batch, err
}

func (s *Server) getMediaBatch(ctx context.Context, req *mcp.CallToolRequest, in mediaBatchResultInput) (*mcp.CallToolResult, media.Batch, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, media.Batch{}, err
	}
	wait, err := waitDuration(in.WaitSeconds, 0)
	if err != nil {
		return nil, media.Batch{}, err
	}
	batch, err := s.media.GetBatch(ctx, userID, in.Items)
	if err == nil {
		batch, err = s.media.WaitBatch(ctx, userID, batch, wait)
	}
	return nil, batch, err
}

func (s *Server) getAttachment(ctx context.Context, req *mcp.CallToolRequest, in attachmentInput) (*mcp.CallToolResult, media.Attachment, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, media.Attachment{}, err
	}
	a, err := s.media.Metadata(ctx, userID, in.Chat, in.MessageID)
	return nil, a, err
}
func (s *Server) downloadAttachment(ctx context.Context, req *mcp.CallToolRequest, in attachmentInput) (*mcp.CallToolResult, media.Download, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, media.Download{}, err
	}
	d, err := s.media.Download(ctx, userID, in.Chat, in.MessageID)
	if err == nil {
		d.DownloadURL = s.publicURL + "/mcp/media/" + d.FileID
	}
	return nil, d, err
}
func (s *Server) transcribeMedia(ctx context.Context, req *mcp.CallToolRequest, in transcribeInput) (*mcp.CallToolResult, media.Job, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, media.Job{}, err
	}
	j, err := s.media.Start(ctx, userID, in.Chat, in.MessageID, "transcription", media.Options{Model: in.Model, Keywords: in.Keywords, Languages: in.Languages}, in.ConfirmPaid)
	return nil, j, err
}
func (s *Server) analyzeImage(ctx context.Context, req *mcp.CallToolRequest, in imageInput) (*mcp.CallToolResult, media.Job, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, media.Job{}, err
	}
	j, err := s.media.Start(ctx, userID, in.Chat, in.MessageID, "image", media.Options{Model: in.Model, DescriptionLanguage: in.DescriptionLanguage}, in.ConfirmPaid)
	return nil, j, err
}
func (s *Server) getTranscription(ctx context.Context, req *mcp.CallToolRequest, in mediaResultInput) (*mcp.CallToolResult, media.Job, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, media.Job{}, err
	}
	j, err := s.media.Get(ctx, userID, in.Chat, in.MessageID, in.JobID)
	if err == nil && j.Operation != "transcription" {
		return nil, media.Job{}, media.Fail("job_operation_mismatch")
	}
	return nil, j, err
}
func (s *Server) getImageAnalysis(ctx context.Context, req *mcp.CallToolRequest, in mediaResultInput) (*mcp.CallToolResult, media.Job, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, media.Job{}, err
	}
	j, err := s.media.Get(ctx, userID, in.Chat, in.MessageID, in.JobID)
	if err == nil && j.Operation != "image" {
		return nil, media.Job{}, media.Fail("job_operation_mismatch")
	}
	return nil, j, err
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.media == nil {
		http.NotFound(w, r)
		return
	}
	userID, err := parsePrincipal(auth.TokenInfoFromContext(r.Context()))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	file, meta, err := s.media.OpenDownload(r.Context(), userID, strings.TrimPrefix(r.URL.Path, "/mcp/media/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	// Never use an untrusted Telegram filename or MIME as an HTTP header.
	ext := ".bin"
	switch meta.MIME {
	case "audio/ogg":
		ext = ".ogg"
	case "video/mp4":
		ext = ".mp4"
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="telegram-%d%s"`, meta.MessageID, ext))
	http.ServeContent(w, r, "", time.Time{}, file)
}
