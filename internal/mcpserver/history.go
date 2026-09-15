package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nextster/telegram-bridge/internal/media"
	"github.com/nextster/telegram-bridge/internal/monitor"
)

func (s *Server) getHistory(ctx context.Context, req *mcp.CallToolRequest, in getHistoryInput) (*mcp.CallToolResult, historyOutput, error) {
	userID, err := principal(req)
	if err != nil {
		return nil, historyOutput{}, err
	}
	minDate, err := parseDate(in.MinDate)
	if err != nil {
		return nil, historyOutput{}, err
	}
	maxDate, err := parseDate(in.MaxDate)
	if err != nil {
		return nil, historyOutput{}, err
	}
	if err := media.ValidateReference(in.Chat, 1); err != nil {
		return nil, historyOutput{}, err
	}
	if in.Limit < 0 || in.Limit > 100 || in.OffsetID < 0 || (!maxDate.IsZero() && !minDate.IsZero() && !minDate.Before(maxDate)) {
		return nil, historyOutput{}, media.Fail("invalid_history_range")
	}
	wait, err := waitDuration(in.WaitSeconds, media.MaxWait)
	if err != nil {
		return nil, historyOutput{}, err
	}
	if in.ProcessMedia {
		if !in.ConfirmPaid || minDate.IsZero() {
			return nil, historyOutput{}, media.Fail("paid_history_requires_confirmation_and_min_date")
		}
		if s.media == nil {
			return nil, historyOutput{}, media.Fail("media_unavailable")
		}
		if maxDate.IsZero() {
			maxDate = time.Now().UTC().Truncate(time.Second).Add(time.Second)
		}
	}
	account, err := s.account(req)
	if err != nil {
		return nil, historyOutput{}, err
	}
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	page, err := account.GetHistoryPage(readCtx, monitor.HistoryOptions{Chat: in.Chat, Limit: in.Limit, OffsetID: in.OffsetID, MinDate: minDate, MaxDate: maxDate})
	cancel()
	if err != nil {
		return nil, historyOutput{}, err
	}
	out := historyOutput{Messages: page.Messages, HasMore: page.HasMore, NextOffsetID: page.NextOffsetID}
	if !minDate.IsZero() {
		out.MinDate = minDate.Format(time.RFC3339Nano)
	}
	if !maxDate.IsZero() {
		out.MaxDate = maxDate.Format(time.RFC3339Nano)
	}
	if !in.ProcessMedia {
		return nil, out, nil
	}
	refs := make([]media.Reference, 0, len(page.Messages))
	for _, message := range page.Messages {
		switch message.MediaKind {
		case "voice", "video_note", "photo", "image":
			refs = append(refs, media.Reference{Chat: message.Chat.Key, MessageID: message.ID})
		case "", "text", "web_page":
		default:
			out.SkippedMedia = append(out.SkippedMedia, skippedMedia{Chat: message.Chat.Key, MessageID: message.ID, Kind: message.MediaKind, Reason: "unsupported_attachment"})
		}
	}
	batch := media.Batch{Items: []media.BatchItem{}, Settled: true, AllSucceeded: true}
	if len(refs) > 0 {
		batch, err = s.media.StartBatch(ctx, userID, refs, media.BatchOptions{Audio: in.Audio, Image: in.Image}, true)
		if err != nil {
			return nil, out, err
		}
		batch, err = s.media.WaitBatch(ctx, userID, batch, wait)
		if err != nil {
			return nil, out, err
		}
	}
	out.Media = &batch
	return nil, out, nil
}

func waitDuration(seconds *int, fallback time.Duration) (time.Duration, error) {
	if seconds == nil {
		return fallback, nil
	}
	if *seconds < 0 || *seconds > int(media.MaxWait/time.Second) {
		return 0, media.Fail("invalid_wait")
	}
	return time.Duration(*seconds) * time.Second, nil
}
