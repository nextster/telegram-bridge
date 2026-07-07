package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/nextster/tg-radar/internal/config"
	"github.com/nextster/tg-radar/internal/db"
	"github.com/nextster/tg-radar/internal/monitor"
)

type Service struct {
	bot            *telego.Bot
	store          *db.Store
	cfg            config.Config
	monitorService *monitor.Service
	login          *loginManager
}

func New(cfg config.Config, store *db.Store) (*Service, error) {
	if strings.TrimSpace(cfg.BotToken) == "" {
		return nil, errors.New("bot token is empty")
	}
	b, err := telego.NewBot(cfg.BotToken)
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}
	service := &Service{bot: b, store: store, cfg: cfg}
	service.login = newLoginManager(service)
	return service, nil
}

func (s *Service) SetMonitorService(monitorService *monitor.Service) {
	s.monitorService = monitorService
}

func (s *Service) Run(ctx context.Context) error {
	if err := s.setCommands(ctx); err != nil {
		log.Printf("bot commands setup failed: %v", err)
	}

	updates, err := s.bot.UpdatesViaLongPolling(ctx, nil)
	if err != nil {
		return fmt.Errorf("start bot long polling: %w", err)
	}

	log.Print("telegram bot polling started")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if err := s.handleUpdate(ctx, update); err != nil {
				log.Printf("bot update failed: %v", err)
			}
		}
	}
}

func (s *Service) NotifyEvent(ctx context.Context, event db.Event) error {
	subscribers, err := s.store.ListSubscribers(ctx)
	if err != nil {
		return err
	}
	if len(subscribers) == 0 {
		return nil
	}

	body := formatEvent(event)
	markup := s.eventMarkup(ctx, event)
	for _, sub := range subscribers {
		_, sendErr := s.bot.SendMessage(ctx, &telego.SendMessageParams{
			ChatID:      telego.ChatID{ID: sub.ChatID},
			Text:        body,
			ReplyMarkup: markup,
		})
		if sendErr != nil {
			log.Printf("send alert to %d failed: %v", sub.ChatID, sendErr)
		}
	}
	return nil
}

func (s *Service) NotifySystem(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	subscribers, err := s.store.ListSubscribers(ctx)
	if err != nil {
		return err
	}
	for _, sub := range subscribers {
		_, sendErr := s.bot.SendMessage(ctx, &telego.SendMessageParams{
			ChatID: telego.ChatID{ID: sub.ChatID},
			Text:   text,
		})
		if sendErr != nil {
			log.Printf("send system notification to %d failed: %v", sub.ChatID, sendErr)
		}
	}
	return nil
}

func (s *Service) setCommands(ctx context.Context) error {
	return s.bot.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "start", Description: "subscribe to alerts"},
			{Command: "stop", Description: "unsubscribe from alerts"},
			{Command: "add", Description: "add keyword"},
			{Command: "del", Description: "delete keyword by id or text"},
			{Command: "keywords", Description: "list keywords"},
			{Command: "recent", Description: "show recent matches"},
			{Command: "login", Description: "authorize Telegram user session"},
			{Command: "loginstatus", Description: "show Telegram user login status"},
			{Command: "cancel", Description: "cancel current bot login"},
			{Command: "help", Description: "show commands"},
		},
	})
}

func (s *Service) handleUpdate(ctx context.Context, update telego.Update) error {
	if update.CallbackQuery != nil {
		return s.handleCallbackQuery(ctx, update.CallbackQuery)
	}
	if update.Message == nil {
		return nil
	}
	message := update.Message
	text := strings.TrimSpace(message.Text)
	if s.login != nil {
		consumed, err := s.login.HandleMessage(ctx, message)
		if consumed || err != nil {
			return err
		}
	}
	if text == "" || !strings.HasPrefix(text, "/") {
		return nil
	}

	command, _, payload := tu.ParseCommandPayload(text)
	switch command {
	case "start":
		return s.handleStart(ctx, message)
	case "stop":
		return s.handleStop(ctx, message)
	case "add":
		return s.handleAddKeyword(ctx, message, payload)
	case "del", "delete":
		return s.handleDeleteKeyword(ctx, message, payload)
	case "keywords":
		return s.handleKeywords(ctx, message)
	case "recent":
		return s.handleRecent(ctx, message)
	case "login":
		return s.login.Start(ctx, message, payload)
	case "loginstatus":
		return s.handleLoginStatus(ctx, message)
	case "cancel":
		return s.login.Cancel(ctx, message.Chat.ID)
	case "help":
		return s.reply(ctx, message.Chat.ID, helpText(), nil)
	default:
		return s.reply(ctx, message.Chat.ID, "Unknown command. Send /help.", nil)
	}
}

func (s *Service) handleCallbackQuery(ctx context.Context, query *telego.CallbackQuery) error {
	if query == nil {
		return nil
	}
	data := strings.TrimSpace(query.Data)
	if strings.HasPrefix(data, "kwdel:") {
		return s.handleStopKeywordCallback(ctx, query, strings.TrimPrefix(data, "kwdel:"))
	}
	return s.answerCallback(ctx, query.ID, "Unknown action.")
}

func (s *Service) handleStopKeywordCallback(ctx context.Context, query *telego.CallbackQuery, rawEventID string) error {
	eventID, err := strconv.ParseInt(strings.TrimSpace(rawEventID), 10, 64)
	if err != nil || eventID <= 0 {
		return s.answerCallback(ctx, query.ID, "This button is stale.")
	}

	event, ok, err := s.store.GetEvent(ctx, eventID)
	if err != nil {
		_ = s.answerCallback(ctx, query.ID, "Could not stop keyword.")
		return err
	}
	if !ok || strings.TrimSpace(event.Keyword) == "" {
		return s.answerCallback(ctx, query.ID, "This match is no longer available.")
	}

	rows, err := s.store.DeleteKeyword(ctx, event.Keyword)
	if err != nil {
		_ = s.answerCallback(ctx, query.ID, "Could not stop keyword.")
		return err
	}

	text := fmt.Sprintf("Stopped keyword: %s", event.Keyword)
	if rows == 0 {
		text = fmt.Sprintf("Keyword already stopped: %s", event.Keyword)
	}
	if err := s.answerCallback(ctx, query.ID, text); err != nil {
		log.Printf("answer stop keyword callback failed: %v", err)
	}
	s.clearCallbackMarkup(ctx, query)
	return s.sendCallbackConfirmation(ctx, query, text)
}

func (s *Service) answerCallback(ctx context.Context, queryID, text string) error {
	if strings.TrimSpace(queryID) == "" {
		return nil
	}
	return s.bot.AnswerCallbackQuery(ctx, tu.CallbackQuery(queryID).WithText(text))
}

func (s *Service) clearCallbackMarkup(ctx context.Context, query *telego.CallbackQuery) {
	if query == nil || query.Message == nil {
		return
	}
	chat := query.Message.GetChat()
	messageID := query.Message.GetMessageID()
	if chat.ID == 0 || messageID == 0 {
		return
	}
	if _, err := s.bot.EditMessageReplyMarkup(ctx, tu.EditMessageReplyMarkup(telego.ChatID{ID: chat.ID}, messageID, nil)); err != nil {
		log.Printf("clear callback markup failed: %v", err)
	}
}

func (s *Service) sendCallbackConfirmation(ctx context.Context, query *telego.CallbackQuery, text string) error {
	if query == nil || query.Message == nil {
		return nil
	}
	chatID := query.Message.GetChat().ID
	if chatID == 0 {
		return nil
	}
	return s.reply(ctx, chatID, text, nil)
}

func (s *Service) handleStart(ctx context.Context, message *telego.Message) error {
	var username, firstName, lastName string
	if message.From != nil {
		username = message.From.Username
		firstName = message.From.FirstName
		lastName = message.From.LastName
	} else {
		username = message.Chat.Username
		firstName = message.Chat.FirstName
		lastName = message.Chat.LastName
	}
	if err := s.store.UpsertSubscriber(ctx, db.Subscriber{
		ChatID:    message.Chat.ID,
		Username:  username,
		FirstName: firstName,
		LastName:  lastName,
	}); err != nil {
		return err
	}

	stats, err := s.store.Stats(ctx)
	if err != nil {
		return err
	}

	text := fmt.Sprintf("Subscribed to tg-radar alerts.\n\nKeywords: %d\nMatches stored: %d\n\nUse /add keyword to add a radar phrase.", stats.Keywords, stats.Events)
	if len(s.cfg.BotAdminChatIDs) == 0 {
		if first, ok, err := s.store.FirstSubscriber(ctx); err == nil && ok && first.ChatID == message.Chat.ID {
			text += "\n\nThis chat is the bot admin chat. Use /login to authorize Telegram monitoring."
		}
	}
	return s.reply(ctx, message.Chat.ID, text, s.webAppMarkup())
}

func (s *Service) handleStop(ctx context.Context, message *telego.Message) error {
	if err := s.store.DeleteSubscriber(ctx, message.Chat.ID); err != nil {
		return err
	}
	return s.reply(ctx, message.Chat.ID, "Unsubscribed from tg-radar alerts.", nil)
}

func (s *Service) handleAddKeyword(ctx context.Context, message *telego.Message, payload string) error {
	keyword, err := s.store.AddKeyword(ctx, payload)
	if err != nil {
		return s.reply(ctx, message.Chat.ID, "Usage: /add keyword", nil)
	}
	return s.reply(ctx, message.Chat.ID, fmt.Sprintf("Added keyword #%d: %s", keyword.ID, keyword.Phrase), nil)
}

func (s *Service) handleDeleteKeyword(ctx context.Context, message *telego.Message, payload string) error {
	rows, err := s.store.DeleteKeyword(ctx, payload)
	if err != nil || rows == 0 {
		return s.reply(ctx, message.Chat.ID, "Usage: /del keyword-or-id", nil)
	}
	return s.reply(ctx, message.Chat.ID, "Deleted keyword.", nil)
}

func (s *Service) handleKeywords(ctx context.Context, message *telego.Message) error {
	keywords, err := s.store.ListKeywords(ctx)
	if err != nil {
		return err
	}
	if len(keywords) == 0 {
		return s.reply(ctx, message.Chat.ID, "No keywords yet. Use /add keyword.", nil)
	}

	var b strings.Builder
	b.WriteString("Keywords:\n")
	for _, keyword := range keywords {
		fmt.Fprintf(&b, "#%d %s\n", keyword.ID, keyword.Phrase)
	}
	return s.reply(ctx, message.Chat.ID, strings.TrimSpace(b.String()), s.webAppMarkup())
}

func (s *Service) handleRecent(ctx context.Context, message *telego.Message) error {
	events, err := s.store.ListEvents(ctx, 5)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return s.reply(ctx, message.Chat.ID, "No matches yet.", nil)
	}

	var b strings.Builder
	b.WriteString("Recent matches:\n")
	for _, event := range events {
		fmt.Fprintf(&b, "\n#%d [%s] %s:%d\n%s", event.ID, event.Keyword, event.SourcePeerType, event.SourcePeerID, truncate(event.Text, 180))
	}
	return s.reply(ctx, message.Chat.ID, strings.TrimSpace(b.String()), s.webAppMarkup())
}

func (s *Service) handleLoginStatus(ctx context.Context, message *telego.Message) error {
	if s.monitorService == nil {
		return s.reply(ctx, message.Chat.ID, "Telegram user API is not configured.", nil)
	}
	status := s.monitorService.Status()
	switch {
	case !status.Configured:
		return s.reply(ctx, message.Chat.ID, "Telegram user API is not configured.", nil)
	case status.Authorized:
		return s.reply(ctx, message.Chat.ID, fmt.Sprintf("Telegram user session is authorized as user_id=%d.", status.UserID), nil)
	case status.LastRunError != "":
		return s.reply(ctx, message.Chat.ID, "Telegram user session is not authorized.\n\nLast monitor error: "+status.LastRunError, nil)
	default:
		return s.reply(ctx, message.Chat.ID, "Telegram user session is not authorized. Use /login to connect it.", nil)
	}
}

func (s *Service) reply(ctx context.Context, chatID int64, text string, markup telego.ReplyMarkup) error {
	_, err := s.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:      telego.ChatID{ID: chatID},
		Text:        text,
		ReplyMarkup: markup,
	})
	return err
}

func (s *Service) webAppMarkup() telego.ReplyMarkup {
	if s.cfg.PublicBaseURL == "" {
		return nil
	}
	return tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("Open dashboard").WithWebApp(&telego.WebAppInfo{URL: s.cfg.PublicBaseURL}),
		),
	)
}

func (s *Service) eventMarkup(ctx context.Context, event db.Event) telego.ReplyMarkup {
	rows := make([][]telego.InlineKeyboardButton, 0, 2)
	if url := s.messageURL(ctx, event); url != "" {
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("Open message").WithURL(url),
		))
	}
	if event.ID > 0 && strings.TrimSpace(event.Keyword) != "" {
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("Stop keyword").WithCallbackData(fmt.Sprintf("kwdel:%d", event.ID)),
		))
	}
	if len(rows) == 0 {
		return nil
	}
	return tu.InlineKeyboard(rows...)
}

func (s *Service) messageURL(ctx context.Context, event db.Event) string {
	peer, ok, err := s.store.GetMonitorPeer(ctx, event.SourcePeerType, event.SourcePeerID)
	if err != nil {
		log.Printf("get monitor peer for event link failed: %v", err)
	}
	if !ok {
		peer = db.MonitorPeer{
			PeerType: event.SourcePeerType,
			PeerID:   event.SourcePeerID,
		}
	}
	return messageURLFromPeer(event, peer)
}

func messageURLFromPeer(event db.Event, peer db.MonitorPeer) string {
	if event.MessageID <= 0 {
		return ""
	}
	username := telegramUsername(peer.Username)
	if username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", username, event.MessageID)
	}
	if event.SourcePeerType != "channel" {
		return ""
	}
	channelID := telegramChannelID(event.SourcePeerID)
	if channelID == "" {
		channelID = telegramChannelID(peer.PeerID)
	}
	if channelID == "" {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%s/%d", channelID, event.MessageID)
}

func telegramUsername(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "@")
	if value == "" || strings.ContainsAny(value, " /?#\t\r\n") {
		return ""
	}
	return value
}

func telegramChannelID(peerID int64) string {
	if peerID == 0 {
		return ""
	}
	value := strconv.FormatInt(peerID, 10)
	value = strings.TrimPrefix(value, "-100")
	value = strings.TrimPrefix(value, "-")
	if value == "" || value == "0" {
		return ""
	}
	return value
}

func formatEvent(event db.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tg-radar match: %s\n", event.Keyword)
	fmt.Fprintf(&b, "Source: %s:%d message %d\n", event.SourcePeerType, event.SourcePeerID, event.MessageID)
	if !event.MessageDate.IsZero() {
		fmt.Fprintf(&b, "Time: %s\n", event.MessageDate.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "\n%s", truncate(event.Text, 2600))
	return b.String()
}

func helpText() string {
	return strings.Join([]string{
		"tg-radar commands:",
		"/start - subscribe this chat to alerts",
		"/stop - unsubscribe this chat",
		"/add keyword - add a radar phrase",
		"/del keyword-or-id - delete a phrase",
		"/keywords - list phrases",
		"/recent - show recent matches",
		"/login - authorize Telegram user monitoring",
		"/loginstatus - show Telegram user session status",
		"/cancel - cancel current login",
	}, "\n")
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "..."
}
