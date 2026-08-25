package monitor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/nextster/telegram-bridge/internal/db"
)

const (
	privateDialogBackfillLimit = 100
	privateHistoryPageLimit    = 100
	privateArchiveRetention    = 90 * 24 * time.Hour
	privateArchiveMaxMessages  = 25_000
	privateArchiveMaintenance  = 24 * time.Hour
	privateOutboxInterval      = 2 * time.Second
	privateDeletionAlertChunk  = 6
)

func (h *Handler) SetSelfUserID(userID int64) {
	h.selfUserID.Store(userID)
}

func (h *Handler) shortMessageSenderID(peerID int64, outgoing bool) int64 {
	if outgoing {
		if selfUserID := h.selfUserID.Load(); selfUserID != 0 {
			return selfUserID
		}
	}
	return peerID
}

func (h *Handler) archivePrivateMessage(ctx context.Context, observed observedMessage) error {
	ownerUserID := h.selfUserID.Load()
	if ownerUserID <= 0 || observed.SourcePeerType != "user" || observed.SourcePeerID <= 0 || observed.MessageID <= 0 {
		return nil
	}
	if observed.SourcePeerID == ownerUserID {
		return nil
	}

	dialog := observed.PrivateDialog
	if dialog.PeerID != 0 {
		dialog.OwnerUserID = ownerUserID
		if err := h.store.UpsertPrivateDialog(ctx, dialog); err != nil {
			return err
		}
	}
	storedDialog, ok, err := h.store.GetPrivateDialog(ctx, ownerUserID, observed.SourcePeerID)
	if err != nil {
		return err
	}
	// Fail closed: a user must be classified as a direct non-bot dialog before
	// any private text is archived.
	if !ok || storedDialog.IsBot {
		return nil
	}

	senderID := observed.SenderPeerID
	if senderID == 0 {
		senderID = h.shortMessageSenderID(observed.SourcePeerID, observed.Outgoing)
	}
	return h.store.UpsertPrivateMessage(ctx, db.PrivateMessage{
		OwnerUserID: ownerUserID,
		MessageID:   observed.MessageID,
		PeerID:      observed.SourcePeerID,
		SenderID:    senderID,
		MessageDate: observed.MessageDate,
		EditDate:    observed.EditDate,
		Text:        observed.Text,
		MediaType:   observed.MediaType,
		Outgoing:    observed.Outgoing,
	})
}

func (h *Handler) handleDeletedPrivateMessages(ctx context.Context, messageIDs []int) error {
	ownerUserID := h.selfUserID.Load()
	if ownerUserID <= 0 {
		return nil
	}
	return h.store.RecordPrivateMessageDeletions(ctx, ownerUserID, messageIDs, time.Now().UTC())
}

func (h *Handler) runPrivateDeletionOutbox(ctx context.Context) {
	ticker := time.NewTicker(privateOutboxInterval)
	defer ticker.Stop()
	for {
		if err := h.flushPrivateDeletionOutbox(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("private deletion outbox flush failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) flushPrivateDeletionOutbox(ctx context.Context) error {
	h.deletionMu.Lock()
	defer h.deletionMu.Unlock()

	ownerUserID := h.selfUserID.Load()
	if ownerUserID <= 0 || h.deletionNotifier == nil {
		return nil
	}
	batches, err := h.store.ListPendingPrivateMessageDeletions(ctx, ownerUserID, 200)
	if err != nil {
		return err
	}
	for _, batch := range batches {
		for start := 0; start < len(batch.Messages); start += privateDeletionAlertChunk {
			end := start + privateDeletionAlertChunk
			if end > len(batch.Messages) {
				end = len(batch.Messages)
			}
			chunk := batch
			chunk.Messages = batch.Messages[start:end]
			messageIDs := privateMessageIDs(chunk.Messages)
			attemptedAt := time.Now().UTC()
			if err := h.deletionNotifier.NotifyDeletedMessages(ctx, chunk); err != nil {
				remainingIDs := privateMessageIDs(batch.Messages[start:])
				retryAt := attemptedAt.Add(privateDeletionRetryBackoff(batch.Attempts + 1))
				if markErr := h.store.MarkPrivateDeletionFailed(ctx, ownerUserID, remainingIDs, attemptedAt, retryAt, err.Error()); markErr != nil {
					return errors.Join(err, markErr)
				}
				return err
			}
			if err := h.store.MarkPrivateDeletionNotified(ctx, ownerUserID, messageIDs, attemptedAt); err != nil {
				return err
			}
		}
	}
	return nil
}

func privateMessageIDs(messages []db.PrivateMessage) []int {
	ids := make([]int, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.MessageID)
	}
	return ids
}

func privateDeletionRetryBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 5 * time.Second
	case attempt == 2:
		return 15 * time.Second
	case attempt == 3:
		return time.Minute
	case attempt == 4:
		return 5 * time.Minute
	case attempt == 5:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

func (s *Service) rememberPrivateDialogs(ctx context.Context, selfUserID int64, dialogs []tg.DialogClass, users []tg.UserClass) error {
	directUserIDs := make(map[int64]struct{})
	for _, item := range dialogs {
		dialog, ok := item.(*tg.Dialog)
		if !ok {
			continue
		}
		peer, ok := dialog.Peer.(*tg.PeerUser)
		if !ok || peer.UserID <= 0 || peer.UserID == selfUserID {
			continue
		}
		directUserIDs[peer.UserID] = struct{}{}
	}

	for peerID, dialog := range privateDialogDirectory(selfUserID, users) {
		if _, ok := directUserIDs[peerID]; !ok {
			continue
		}
		if err := s.store.UpsertPrivateDialog(ctx, dialog); err != nil {
			return err
		}
		if dialog.AccessHash != 0 {
			if err := s.store.SetUserAccessHash(ctx, selfUserID, peerID, dialog.AccessHash); err != nil {
				return err
			}
		}
	}
	return nil
}

func privateDialogDirectory(ownerUserID int64, users []tg.UserClass) map[int64]db.PrivateDialog {
	directory := make(map[int64]db.PrivateDialog, len(users))
	for _, item := range users {
		user, ok := item.(*tg.User)
		if !ok || user.ID <= 0 {
			continue
		}
		username := user.Username
		if value, ok := user.GetUsername(); ok {
			username = value
		}
		accessHash := user.AccessHash
		if value, ok := user.GetAccessHash(); ok {
			accessHash = value
		}
		directory[user.ID] = db.PrivateDialog{
			OwnerUserID: ownerUserID,
			PeerID:      user.ID,
			AccessHash:  accessHash,
			Title:       strings.TrimSpace(strings.Join([]string{user.FirstName, user.LastName}, " ")),
			Username:    username,
			IsBot:       user.Bot,
		}
	}
	return directory
}

func (s *Service) runPrivateArchive(ctx context.Context) {
	s.runPrivateArchivePass(ctx)
	ticker := time.NewTicker(privateArchiveMaintenance)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runPrivateArchivePass(ctx)
		}
	}
}

func (s *Service) runPrivateArchivePass(ctx context.Context) {
	ownerUserID := s.handler.selfUserID.Load()
	cutoff := time.Now().UTC().Add(-privateArchiveRetention)
	if err := s.store.PrunePrivateArchive(ctx, cutoff, privateArchiveMaxMessages); err != nil {
		log.Printf("private archive prune failed: %v", err)
	}
	dialogs, messages, err := s.backfillPrivateMessages(ctx, cutoff)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("private message archive backfill incomplete: dialogs=%d messages=%d: %v", dialogs, messages, err)
	} else if err == nil {
		log.Printf("private message archive backfilled: dialogs=%d messages=%d", dialogs, messages)
	}
	stats, statsErr := s.store.PrivateArchiveStats(ctx, ownerUserID)
	if statsErr != nil {
		log.Printf("private archive stats unavailable: %v", statsErr)
		return
	}
	log.Printf("private archive ready: dialogs=%d messages=%d deleted=%d pending=%d retention_days=%d",
		stats.Dialogs, stats.Messages, stats.Deleted, stats.Pending, int(privateArchiveRetention.Hours()/24))
}

func (s *Service) backfillPrivateMessages(ctx context.Context, cutoff time.Time) (int, int, error) {
	api, selfUserID, err := s.readyAPI()
	if err != nil {
		return 0, 0, err
	}
	dialogs, err := s.store.ListPrivateDialogsNeedingBackfill(ctx, selfUserID, privateDialogBackfillLimit)
	if err != nil {
		return 0, 0, err
	}

	var completed, archived int
	var failures []error
	for _, dialog := range dialogs {
		dialogFailed := false
		if err := ctx.Err(); err != nil {
			return completed, archived, err
		}
		// Seed only the latest page. From this point onward live updates keep the
		// 90-day archive current; bounding the seed protects the small Fly volume.
		history, err := s.getPrivateHistory(ctx, api, dialog, 0)
		if err != nil {
			failures = append(failures, err)
			dialogFailed = true
		} else {
			for _, item := range messagesFromHistory(history) {
				message, ok := item.(*tg.Message)
				if !ok {
					continue
				}
				messageDate := unixTime(message.Date)
				if messageDate.Before(cutoff) {
					break
				}
				_, senderID := peerInfo(message.FromID)
				if err := s.handler.archivePrivateMessage(ctx, observedMessage{
					SourcePeerType: "user",
					SourcePeerID:   dialog.PeerID,
					SenderPeerID:   senderID,
					MessageID:      message.ID,
					MessageDate:    messageDate,
					EditDate:       optionalUnixTime(message.EditDate),
					Text:           message.Message,
					MediaType:      telegramMediaType(message.Media),
					Outgoing:       message.Out,
					PrivateDialog:  dialog,
				}); err != nil {
					failures = append(failures, fmt.Errorf("archive private dialog %d message %d: %w", dialog.PeerID, message.ID, err))
					dialogFailed = true
					continue
				}
				archived++
			}
		}
		if dialogFailed {
			continue
		}
		if err := s.store.SetPrivateDialogBackfilled(ctx, selfUserID, dialog.PeerID, time.Now().UTC()); err != nil {
			failures = append(failures, err)
			continue
		}
		completed++
	}
	return completed, archived, errors.Join(failures...)
}

func (s *Service) getPrivateHistory(ctx context.Context, api *tg.Client, dialog db.PrivateDialog, offsetID int) (tg.MessagesMessagesClass, error) {
	for attempt := 0; attempt < 3; attempt++ {
		history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:     &tg.InputPeerUser{UserID: dialog.PeerID, AccessHash: dialog.AccessHash},
			OffsetID: offsetID,
			Limit:    privateHistoryPageLimit,
		})
		if err == nil {
			return history, nil
		}
		wait, ok := backfillFloodWait(err)
		if !ok {
			return nil, fmt.Errorf("get private history for user:%d: %w", dialog.PeerID, err)
		}
		if wait > maxBackfillFloodWait {
			return nil, fmt.Errorf("telegram rate limit for user:%d: wait %s and retry", dialog.PeerID, wait.Round(time.Second))
		}
		log.Printf("telegram private history flood wait for user:%d: waiting %s", dialog.PeerID, wait.Round(time.Second))
		if err := sleepContext(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("telegram rate limit for user:%d: retry later", dialog.PeerID)
}

func telegramMediaType(media tg.MessageMediaClass) string {
	switch media.(type) {
	case nil, *tg.MessageMediaEmpty, *tg.MessageMediaWebPage:
		return ""
	case *tg.MessageMediaPhoto:
		return "photo"
	case *tg.MessageMediaDocument:
		return "file"
	case *tg.MessageMediaContact:
		return "contact"
	case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive, *tg.MessageMediaVenue:
		return "location"
	case *tg.MessageMediaPoll:
		return "poll"
	case *tg.MessageMediaDice:
		return "dice"
	case *tg.MessageMediaGame:
		return "game"
	default:
		return "media"
	}
}
