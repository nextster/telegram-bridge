package monitor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/match"
	"github.com/nextster/telegram-bridge/internal/notify"
)

type Handler struct {
	store            *db.Store
	notifier         notify.Notifier
	deletionNotifier notify.DeletionNotifier
	selfUserID       atomic.Int64
	deletionMu       sync.Mutex
}

type Service struct {
	cfg      config.Config
	store    *db.Store
	notifier notify.Notifier
	handler  *Handler

	mu           sync.RWMutex
	api          *tg.Client
	userID       int64
	authorized   bool
	lastRunError string

	backfillMu sync.Mutex
	reloadCh   chan struct{}
}

type Status struct {
	Configured   bool
	Authorized   bool
	UserID       int64
	LastRunError string
}

type BackfillResult struct {
	Peers    int
	Scanned  int
	Matched  int
	Inserted int
	Since    time.Time
}

type ProcessResult struct {
	Matched  int
	Inserted int
}

type LoginPromptKind string

const (
	LoginPromptPhone    LoginPromptKind = "phone"
	LoginPromptCode     LoginPromptKind = "code"
	LoginPromptPassword LoginPromptKind = "password"
)

type LoginPromptRequest struct {
	Kind    LoginPromptKind
	Message string
}

type LoginPromptFunc func(ctx context.Context, req LoginPromptRequest) (string, error)

type LoginOptions struct {
	Phone  string
	Prompt LoginPromptFunc
}

type LoginResult struct {
	UserID            int64
	SessionPath       string
	AlreadyAuthorized bool
}

var errReloadRequested = errors.New("monitor reload requested")

const (
	backfillPageDelay     = 1500 * time.Millisecond
	maxBackfillFloodWait  = 2 * time.Minute
	floodWaitSafetyMargin = time.Second
)

func NewHandler(store *db.Store, notifier notify.Notifier) *Handler {
	if notifier == nil {
		notifier = notify.Nop{}
	}
	deletionNotifier, ok := notifier.(notify.DeletionNotifier)
	if !ok {
		deletionNotifier = nil
	}
	switch notifier.(type) {
	case notify.Nop, *notify.Nop:
		deletionNotifier = nil
	}
	return &Handler{store: store, notifier: notifier, deletionNotifier: deletionNotifier}
}

func NewService(cfg config.Config, store *db.Store, notifier notify.Notifier) *Service {
	handler := NewHandler(store, notifier)
	return &Service{
		cfg:      cfg,
		store:    store,
		notifier: notifier,
		handler:  handler,
		reloadCh: make(chan struct{}, 1),
	}
}

func Run(ctx context.Context, cfg config.Config, store *db.Store, notifier notify.Notifier) error {
	return NewService(cfg, store, notifier).Run(ctx)
}

func (s *Service) Run(ctx context.Context) error {
	for {
		err := s.runOnce(ctx)
		if errors.Is(err, errReloadRequested) {
			continue
		}
		return err
	}
}

func (s *Service) runOnce(ctx context.Context) error {
	cfg := s.cfg
	if !cfg.HasTelegramUserAPI() {
		log.Print("telegram user API monitoring disabled: TELEGRAM_API_ID/TELEGRAM_API_HASH are not configured")
		<-ctx.Done()
		return nil
	}
	if err := ensureParentDir(cfg.SessionPath); err != nil {
		return err
	}

	manager := updates.New(updates.Config{
		Handler:          s.handler,
		Storage:          s.store,
		AccessHasher:     s.store,
		UserAccessHasher: s.store,
	})
	client := telegram.NewClient(cfg.TelegramAPIID, cfg.TelegramAPIHash, telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{Path: cfg.SessionPath},
		UpdateHandler:  manager,
	})

	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check Telegram user auth status: %w", err)
		}
		if !status.Authorized || status.User == nil {
			log.Print("telegram user API monitoring disabled: session is not authorized; run `telegram-bridge login` first")
			s.setState(nil, 0, false, "")
			select {
			case <-ctx.Done():
				return nil
			case <-s.reloadCh:
				return errReloadRequested
			}
		}

		s.setState(client.API(), status.User.ID, true, "")
		s.handler.SetSelfUserID(status.User.ID)
		log.Printf("telegram user monitoring started as user_id=%d", status.User.ID)
		err = manager.Run(ctx, client.API(), status.User.ID, updates.AuthOptions{
			OnStart: func(managerCtx context.Context) {
				go s.handler.runPrivateDeletionOutbox(managerCtx)
				go s.runCodexReadReconcile(managerCtx)
				go func() {
					if count, syncErr := s.SyncDialogs(managerCtx); syncErr != nil {
						log.Printf("telegram dialogs sync failed: %v", syncErr)
					} else {
						log.Printf("telegram dialogs synced: %d sources", count)
					}
					s.runPrivateArchive(managerCtx)
				}()
			},
		})
		if errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			s.setLastRunError(err.Error())
		}
		return err
	})
}

func (s *Service) Reload() {
	select {
	case s.reloadCh <- struct{}{}:
	default:
	}
}

func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Status{
		Configured:   s.cfg.HasTelegramUserAPI(),
		Authorized:   s.authorized,
		UserID:       s.userID,
		LastRunError: s.lastRunError,
	}
}

func (s *Service) SyncDialogs(ctx context.Context) (int, error) {
	api, userID, err := s.readyAPI()
	if err != nil {
		return 0, err
	}

	result, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      100,
	})
	if err != nil {
		return 0, fmt.Errorf("get dialogs: %w", err)
	}

	dialogs, chats, users := dialogParts(result)
	count := 0
	for _, chat := range chats {
		peer, ok := monitorPeerFromChat(chat)
		if !ok {
			continue
		}
		if err := s.store.UpsertMonitorPeer(ctx, peer); err != nil {
			return count, err
		}
		if peer.PeerType == "channel" && peer.AccessHash != 0 {
			if err := s.store.SetChannelAccessHash(ctx, userID, peer.PeerID, peer.AccessHash); err != nil {
				return count, err
			}
		}
		count++
	}
	if err := s.rememberPrivateDialogs(ctx, userID, dialogs, users); err != nil {
		return count, err
	}
	return count, nil
}

func (s *Service) Backfill(ctx context.Context, days int) (BackfillResult, error) {
	if days <= 0 {
		return BackfillResult{}, errors.New("days must be greater than zero")
	}
	if days > 365 {
		return BackfillResult{}, errors.New("days must be 365 or less")
	}

	api, _, err := s.readyAPI()
	if err != nil {
		return BackfillResult{}, err
	}

	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()

	peers, err := s.store.ListMonitorPeers(ctx, true)
	if err != nil {
		return BackfillResult{}, err
	}
	result := BackfillResult{
		Peers: len(peers),
		Since: time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour),
	}

	for _, peer := range peers {
		peerResult, err := s.backfillPeer(ctx, api, peer, result.Since)
		if err != nil {
			return result, err
		}
		result.Scanned += peerResult.Scanned
		result.Matched += peerResult.Matched
		result.Inserted += peerResult.Inserted
		if err := s.store.SetMonitorPeerBackfilled(ctx, peer.PeerType, peer.PeerID, time.Now().UTC()); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) backfillPeer(ctx context.Context, api *tg.Client, peer db.MonitorPeer, since time.Time) (BackfillResult, error) {
	inputPeer, ok := inputPeerFromMonitorPeer(peer)
	if !ok {
		return BackfillResult{}, nil
	}

	var result BackfillResult
	offsetID := 0
	for page := 0; page < 100; page++ {
		history, err := s.getHistory(ctx, api, peer, &tg.MessagesGetHistoryRequest{
			Peer:     inputPeer,
			OffsetID: offsetID,
			Limit:    100,
		})
		if err != nil {
			return result, err
		}

		messages := messagesFromHistory(history)
		if len(messages) == 0 {
			return result, nil
		}

		nextOffsetID := 0
		for _, message := range messages {
			if id := message.GetID(); id > 0 {
				nextOffsetID = id
			}
			msg, ok := message.(*tg.Message)
			if !ok {
				continue
			}
			messageTime := unixTime(msg.Date)
			if messageTime.Before(since) {
				return result, nil
			}
			result.Scanned++
			processed, err := s.handler.processObserved(ctx, observedMessage{
				SourcePeerType: peer.PeerType,
				SourcePeerID:   peer.PeerID,
				MessageID:      msg.ID,
				MessageDate:    messageTime,
				Text:           msg.Message,
			}, false)
			if err != nil {
				return result, err
			}
			result.Matched += processed.Matched
			result.Inserted += processed.Inserted
		}
		if nextOffsetID == 0 || nextOffsetID == offsetID {
			return result, nil
		}
		offsetID = nextOffsetID
		if err := sleepContext(ctx, backfillPageDelay); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) getHistory(ctx context.Context, api *tg.Client, peer db.MonitorPeer, req *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error) {
	for attempt := 0; attempt < 3; attempt++ {
		history, err := api.MessagesGetHistory(ctx, req)
		if err == nil {
			return history, nil
		}
		wait, ok := backfillFloodWait(err)
		if !ok {
			return nil, fmt.Errorf("get history for %s:%d: %w", peer.PeerType, peer.PeerID, err)
		}
		if wait > maxBackfillFloodWait {
			return nil, fmt.Errorf("telegram rate limit for %s:%d: wait %s and retry", peer.PeerType, peer.PeerID, wait.Round(time.Second))
		}
		log.Printf("telegram history flood wait for %s:%d: waiting %s", peer.PeerType, peer.PeerID, wait.Round(time.Second))
		if err := sleepContext(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("telegram rate limit for %s:%d: retry later", peer.PeerType, peer.PeerID)
}

func backfillFloodWait(err error) (time.Duration, bool) {
	wait, ok := tgerr.AsFloodWait(err)
	if !ok {
		return 0, false
	}
	if wait < 0 {
		wait = 0
	}
	return wait + floodWaitSafetyMargin, true
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) readyAPI() (*tg.Client, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.cfg.HasTelegramUserAPI() {
		return nil, 0, errors.New("telegram user API is not configured")
	}
	if !s.authorized || s.api == nil {
		return nil, 0, errors.New("telegram user session is not authorized; run telegram-bridge login")
	}
	return s.api, s.userID, nil
}

func (s *Service) setState(api *tg.Client, userID int64, authorized bool, lastRunError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.api = api
	s.userID = userID
	s.authorized = authorized
	s.lastRunError = lastRunError
}

func (s *Service) setLastRunError(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRunError = value
}

func (s *Service) LoginWithPrompts(ctx context.Context, opts LoginOptions) (LoginResult, error) {
	cfg := s.cfg
	if !cfg.HasTelegramUserAPI() {
		return LoginResult{}, errors.New("telegram user API is not configured")
	}
	if opts.Prompt == nil {
		return LoginResult{}, errors.New("login prompt is required")
	}
	if err := ensureParentDir(cfg.SessionPath); err != nil {
		return LoginResult{}, err
	}

	client := telegram.NewClient(cfg.TelegramAPIID, cfg.TelegramAPIHash, telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{Path: cfg.SessionPath},
	})

	var result LoginResult
	err := client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check Telegram auth status: %w", err)
		}
		if status.Authorized {
			result.SessionPath = cfg.SessionPath
			result.AlreadyAuthorized = true
			if status.User != nil {
				result.UserID = status.User.ID
			}
			return nil
		}

		userAuth := &promptAuthenticator{
			phone:  strings.TrimSpace(opts.Phone),
			prompt: opts.Prompt,
		}
		if err := auth.NewFlow(userAuth, auth.SendCodeOptions{}).Run(ctx, client.Auth()); err != nil {
			return fmt.Errorf("telegram login flow: %w", err)
		}

		status, err = client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check Telegram auth status after login: %w", err)
		}
		result.SessionPath = cfg.SessionPath
		if status.User != nil {
			result.UserID = status.User.ID
		}
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}
	s.Reload()
	return result, nil
}

type promptAuthenticator struct {
	phone  string
	prompt LoginPromptFunc
}

func (a *promptAuthenticator) Phone(ctx context.Context) (string, error) {
	if a.phone != "" {
		return a.phone, nil
	}
	return a.ask(ctx, LoginPromptPhone, "Send the Telegram phone number in international format, for example +995...")
}

func (a *promptAuthenticator) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	return a.ask(ctx, LoginPromptCode, "Telegram sent a login code. Send that code here.")
}

func (a *promptAuthenticator) Password(ctx context.Context) (string, error) {
	return a.ask(ctx, LoginPromptPassword, "This account has 2FA enabled. Send the Telegram cloud password here.")
}

func (a *promptAuthenticator) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errors.New("Telegram terms of service acceptance is not supported by bot login")
}

func (a *promptAuthenticator) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("Telegram sign-up is not supported")
}

func (a *promptAuthenticator) ask(ctx context.Context, kind LoginPromptKind, message string) (string, error) {
	value, err := a.prompt(ctx, LoginPromptRequest{Kind: kind, Message: message})
	if err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty login value")
	}
	return value, nil
}

func Login(ctx context.Context, cfg config.Config, in io.Reader, out io.Writer) error {
	if err := cfg.ValidateLogin(); err != nil {
		return err
	}
	if err := ensureParentDir(cfg.SessionPath); err != nil {
		return err
	}

	client := telegram.NewClient(cfg.TelegramAPIID, cfg.TelegramAPIHash, telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{Path: cfg.SessionPath},
	})

	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check Telegram auth status: %w", err)
		}
		if status.Authorized {
			fmt.Fprintf(out, "Already authorized as user_id=%d\n", status.User.ID)
			return nil
		}

		reader := bufio.NewReader(in)
		codeAuth := auth.CodeAuthenticatorFunc(func(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
			fmt.Fprint(out, "Telegram code: ")
			code, err := reader.ReadString('\n')
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(code), nil
		})

		var userAuth auth.UserAuthenticator
		if cfg.TelegramPassword != "" {
			userAuth = auth.Constant(cfg.TelegramPhone, cfg.TelegramPassword, codeAuth)
		} else {
			userAuth = auth.CodeOnly(cfg.TelegramPhone, codeAuth)
		}
		if err := auth.NewFlow(userAuth, auth.SendCodeOptions{}).Run(ctx, client.Auth()); err != nil {
			return fmt.Errorf("telegram login flow: %w", err)
		}

		status, err = client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check Telegram auth status after login: %w", err)
		}
		if status.User != nil {
			fmt.Fprintf(out, "Authorized as user_id=%d. Session saved to %s\n", status.User.ID, cfg.SessionPath)
		} else {
			fmt.Fprintf(out, "Authorized. Session saved to %s\n", cfg.SessionPath)
		}
		return nil
	})
}

func (h *Handler) Handle(ctx context.Context, update tg.UpdatesClass) error {
	switch typed := update.(type) {
	case *tg.Updates:
		privateDialogs := privateDialogDirectory(h.selfUserID.Load(), typed.Users)
		for _, item := range typed.Updates {
			if err := h.handleUpdate(ctx, item, privateDialogs); err != nil {
				return err
			}
		}
	case *tg.UpdatesCombined:
		privateDialogs := privateDialogDirectory(h.selfUserID.Load(), typed.Users)
		for _, item := range typed.Updates {
			if err := h.handleUpdate(ctx, item, privateDialogs); err != nil {
				return err
			}
		}
	case *tg.UpdateShort:
		return h.handleUpdate(ctx, typed.Update, nil)
	case *tg.UpdateShortMessage:
		return h.handleText(ctx, observedMessage{
			SourcePeerType: "user",
			SourcePeerID:   typed.UserID,
			SenderPeerID:   h.shortMessageSenderID(typed.UserID, typed.Out),
			MessageID:      typed.ID,
			MessageDate:    unixTime(typed.Date),
			Text:           typed.Message,
			Outgoing:       typed.Out,
		})
	case *tg.UpdateShortChatMessage:
		return h.handleText(ctx, observedMessage{
			SourcePeerType: "chat",
			SourcePeerID:   typed.ChatID,
			MessageID:      typed.ID,
			MessageDate:    unixTime(typed.Date),
			Text:           typed.Message,
		})
	}
	return nil
}

func (h *Handler) handleUpdate(ctx context.Context, update tg.UpdateClass, privateDialogs map[int64]db.PrivateDialog) error {
	switch typed := update.(type) {
	case *tg.UpdateReadChannelDiscussionInbox:
		_, err := h.store.ObserveCodexTopicReadState(ctx, botAPIChannelID(typed.ChannelID), typed.TopMsgID, false, typed.ReadMaxID)
		return err
	case *tg.UpdateNewMessage:
		return h.handleMessageClass(ctx, typed.Message, privateDialogs)
	case *tg.UpdateNewChannelMessage:
		return h.handleMessageClass(ctx, typed.Message, privateDialogs)
	case *tg.UpdateEditMessage:
		return h.handleMessageClass(ctx, typed.Message, privateDialogs)
	case *tg.UpdateEditChannelMessage:
		return h.handleMessageClass(ctx, typed.Message, privateDialogs)
	case *tg.UpdateDeleteMessages:
		return h.handleDeletedPrivateMessages(ctx, typed.Messages)
	}
	return nil
}

func (h *Handler) handleMessageClass(ctx context.Context, message tg.MessageClass, privateDialogs map[int64]db.PrivateDialog) error {
	msg, ok := message.(*tg.Message)
	if !ok {
		return nil
	}
	peerType, peerID := peerInfo(msg.PeerID)
	_, senderID := peerInfo(msg.FromID)
	return h.handleText(ctx, observedMessage{
		SourcePeerType: peerType,
		SourcePeerID:   peerID,
		SenderPeerID:   senderID,
		MessageID:      msg.ID,
		MessageDate:    unixTime(msg.Date),
		EditDate:       optionalUnixTime(msg.EditDate),
		Text:           msg.Message,
		MediaType:      telegramMediaType(msg.Media),
		Outgoing:       msg.Out,
		PrivateDialog:  privateDialogs[peerID],
	})
}

func (h *Handler) handleText(ctx context.Context, observed observedMessage) error {
	_, err := h.processObserved(ctx, observed, true)
	return err
}

func (h *Handler) processObserved(ctx context.Context, observed observedMessage, notifyOnInsert bool) (ProcessResult, error) {
	if err := h.archivePrivateMessage(ctx, observed); err != nil {
		return ProcessResult{}, err
	}
	observed.Text = strings.TrimSpace(observed.Text)
	if observed.Text == "" {
		return ProcessResult{}, nil
	}
	enabled, err := h.store.IsMonitorPeerEnabled(ctx, observed.SourcePeerType, observed.SourcePeerID)
	if err != nil {
		return ProcessResult{}, err
	}
	if !enabled {
		return ProcessResult{}, nil
	}
	keywords, err := h.store.ListKeywords(ctx)
	if err != nil {
		return ProcessResult{}, err
	}
	var result ProcessResult
	for _, matched := range match.Evaluate(observed.Text, observed.SourcePeerType, observed.SourcePeerID, keywords) {
		keyword := matched.Keyword
		event, inserted, err := h.store.RecordEvent(ctx, db.Event{
			SourcePeerType: observed.SourcePeerType,
			SourcePeerID:   observed.SourcePeerID,
			MessageID:      observed.MessageID,
			MessageDate:    observed.MessageDate,
			Text:           observed.Text,
			Keyword:        keyword.Phrase,
			RuleID:         keyword.ID,
			MatchReason:    matched.Reason,
			MatchScore:     matched.Score,
			RuleNote:       keyword.Note,
		})
		if err != nil {
			return result, err
		}
		result.Matched++
		if inserted {
			result.Inserted++
			if notifyOnInsert && h.notifier != nil {
				if err := h.notifier.NotifyEvent(ctx, event); err != nil {
					log.Printf("notify event failed: %v", err)
				}
			}
		}
	}
	return result, nil
}

type observedMessage struct {
	SourcePeerType string
	SourcePeerID   int64
	SenderPeerID   int64
	MessageID      int
	MessageDate    time.Time
	EditDate       time.Time
	Text           string
	MediaType      string
	Outgoing       bool
	PrivateDialog  db.PrivateDialog
}

func peerInfo(peer tg.PeerClass) (string, int64) {
	switch typed := peer.(type) {
	case *tg.PeerUser:
		return "user", typed.UserID
	case *tg.PeerChat:
		return "chat", typed.ChatID
	case *tg.PeerChannel:
		return "channel", typed.ChannelID
	default:
		return "unknown", 0
	}
}

func unixTime(seconds int) time.Time {
	if seconds <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(int64(seconds), 0).UTC()
}

func optionalUnixTime(seconds int) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), 0).UTC()
}

func ensureParentDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("path is empty")
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

func chatsFromDialogs(dialogs tg.MessagesDialogsClass) []tg.ChatClass {
	switch typed := dialogs.(type) {
	case *tg.MessagesDialogs:
		return typed.Chats
	case *tg.MessagesDialogsSlice:
		return typed.Chats
	default:
		return nil
	}
}

func messagesFromHistory(messages tg.MessagesMessagesClass) []tg.MessageClass {
	switch typed := messages.(type) {
	case *tg.MessagesMessages:
		return typed.Messages
	case *tg.MessagesMessagesSlice:
		return typed.Messages
	case *tg.MessagesChannelMessages:
		return typed.Messages
	default:
		return nil
	}
}

func monitorPeerFromChat(chat tg.ChatClass) (db.MonitorPeer, bool) {
	switch typed := chat.(type) {
	case *tg.Channel:
		if typed.Left {
			return db.MonitorPeer{}, false
		}
		kind := "channel"
		if typed.Megagroup {
			kind = "supergroup"
		}
		username, _ := typed.GetUsername()
		accessHash, _ := typed.GetAccessHash()
		return db.MonitorPeer{
			PeerType:   "channel",
			PeerID:     typed.ID,
			AccessHash: accessHash,
			Title:      typed.Title,
			Username:   username,
			Kind:       kind,
		}, true
	case *tg.Chat:
		if typed.Left || typed.Deactivated {
			return db.MonitorPeer{}, false
		}
		return db.MonitorPeer{
			PeerType: "chat",
			PeerID:   typed.ID,
			Title:    typed.Title,
			Kind:     "group",
		}, true
	default:
		return db.MonitorPeer{}, false
	}
}

func inputPeerFromMonitorPeer(peer db.MonitorPeer) (tg.InputPeerClass, bool) {
	switch peer.PeerType {
	case "channel":
		if peer.AccessHash == 0 {
			return nil, false
		}
		return &tg.InputPeerChannel{
			ChannelID:  peer.PeerID,
			AccessHash: peer.AccessHash,
		}, true
	case "chat":
		return &tg.InputPeerChat{ChatID: peer.PeerID}, true
	default:
		return nil, false
	}
}
