package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

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
	for _, sub := range subscribers {
		_, sendErr := s.bot.SendMessage(ctx, &telego.SendMessageParams{
			ChatID:      telego.ChatID{ID: sub.ChatID},
			Text:        body,
			ReplyMarkup: s.eventMarkup(ctx, event, sub.ChatID),
		})
		if sendErr != nil {
			log.Printf("send alert to %d failed: %v", sub.ChatID, sendErr)
		}
	}
	return nil
}

func (s *Service) NotifyDeletedMessages(ctx context.Context, deletion db.PrivateMessageDeletion) error {
	chatID, ok, err := s.privateAlertChatID(ctx, deletion.Dialog.OwnerUserID)
	if err != nil {
		return err
	}
	if len(deletion.Messages) == 0 {
		return nil
	}
	if !ok {
		return fmt.Errorf("private deletion alert owner %d has not subscribed to the bot", deletion.Dialog.OwnerUserID)
	}

	_, err = s.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: chatID},
		Text:   formatDeletedMessages(deletion),
	})
	if err != nil {
		return fmt.Errorf("send deletion alert to owner chat %d: %w", chatID, err)
	}
	return nil
}

func (s *Service) privateAlertChatID(ctx context.Context, ownerUserID int64) (int64, bool, error) {
	if ownerUserID <= 0 {
		return 0, false, nil
	}
	subscribers, err := s.store.ListSubscribers(ctx)
	if err != nil {
		return 0, false, err
	}
	for _, subscriber := range subscribers {
		// In a private Bot API chat, chat_id is the user's Telegram ID. Never
		// route archived personal text to another subscriber or to a group.
		if subscriber.ChatID == ownerUserID {
			return ownerUserID, true, nil
		}
	}
	return 0, false, nil
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
			{Command: "watch", Description: "add flexible watch rule"},
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
	if adminOnlyCommand(command) {
		admin, err := s.isAdminChat(ctx, message.Chat.ID)
		if err != nil {
			return err
		}
		if !admin {
			return s.reply(ctx, message.Chat.ID, "This chat is not allowed to manage tg-radar.", nil)
		}
	}
	switch command {
	case "start":
		return s.handleStart(ctx, message)
	case "stop":
		return s.handleStop(ctx, message)
	case "add":
		return s.handleAddKeyword(ctx, message, payload)
	case "watch":
		return s.handleAddWatchRule(ctx, message, payload)
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
		chatID := callbackChatID(query)
		admin, err := s.isAdminChat(ctx, chatID)
		if err != nil {
			return err
		}
		if !admin {
			return s.answerCallback(ctx, query.ID, "This chat is not allowed to manage tg-radar.")
		}
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

	deleteValue := event.Keyword
	if event.RuleID > 0 {
		deleteValue = strconv.FormatInt(event.RuleID, 10)
	}
	rows, err := s.store.DeleteKeyword(ctx, deleteValue)
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
	allowed, err := s.canSubscribe(ctx, message.Chat.ID)
	if err != nil {
		return err
	}
	if !allowed {
		return s.reply(ctx, message.Chat.ID, "This chat is not allowed to subscribe to tg-radar.", nil)
	}

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
	return s.reply(ctx, message.Chat.ID, text, s.webAppMarkup(ctx, message.Chat.ID))
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

func (s *Service) handleAddWatchRule(ctx context.Context, message *telego.Message, payload string) error {
	parts := strings.Split(payload, "::")
	if len(parts) < 2 {
		return s.reply(ctx, message.Chat.ID, "Usage: /watch name :: any term, synonym :: required terms :: excluded terms", nil)
	}
	rule, err := s.store.UpsertWatchRule(ctx, db.Keyword{
		Phrase:       strings.TrimSpace(parts[0]),
		AnyTerms:     splitRuleTerms(parts[1]),
		AllTerms:     splitRulePart(parts, 2),
		ExcludeTerms: splitRulePart(parts, 3),
		Enabled:      true,
	})
	if err != nil {
		return s.reply(ctx, message.Chat.ID, "Could not add watch rule: "+err.Error(), nil)
	}
	return s.reply(ctx, message.Chat.ID, fmt.Sprintf("Watching #%d %s\nany: %s", rule.ID, rule.Phrase, strings.Join(rule.AnyTerms, ", ")), nil)
}

func splitRulePart(parts []string, index int) []string {
	if index >= len(parts) {
		return nil
	}
	return splitRuleTerms(parts[index])
}

func splitRuleTerms(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' })
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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
	b.WriteString("Watch rules:\n")
	for _, keyword := range keywords {
		fmt.Fprintf(&b, "#%d %s", keyword.ID, keyword.Phrase)
		if len(keyword.AnyTerms) > 0 {
			fmt.Fprintf(&b, "\n  any: %s", formatRuleTerms(keyword.AnyTerms, 3))
		}
		if len(keyword.AllTerms) > 0 {
			fmt.Fprintf(&b, "\n  all: %s", formatRuleTerms(keyword.AllTerms, 3))
		}
		if len(keyword.RequiredAnyGroups)+len(keyword.PreferredTerms)+len(keyword.ExcludeTerms) > 0 {
			fmt.Fprintf(&b, "\n  %d required group(s) · %d preferred · %d excluded",
				len(keyword.RequiredAnyGroups), len(keyword.PreferredTerms), len(keyword.ExcludeTerms))
		}
		if keyword.Note != "" {
			fmt.Fprintf(&b, "\n  note: %s", truncate(keyword.Note, 100))
		}
		if keyword.ExcludeCompleteBike {
			b.WriteString("\n  ignores complete-bike listings")
		}
		b.WriteByte('\n')
	}
	return s.reply(ctx, message.Chat.ID, strings.TrimSpace(b.String()), s.webAppMarkup(ctx, message.Chat.ID))
}

func formatRuleTerms(terms []string, limit int) string {
	if limit <= 0 || len(terms) <= limit {
		return strings.Join(terms, ", ")
	}
	return fmt.Sprintf("%s (+%d)", strings.Join(terms[:limit], ", "), len(terms)-limit)
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
	return s.reply(ctx, message.Chat.ID, strings.TrimSpace(b.String()), s.webAppMarkup(ctx, message.Chat.ID))
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

func (s *Service) webAppMarkup(ctx context.Context, chatID int64) telego.ReplyMarkup {
	if s.cfg.PublicBaseURL == "" {
		return nil
	}
	admin, err := s.isAdminChat(ctx, chatID)
	if err != nil {
		log.Printf("check dashboard admin chat %d failed: %v", chatID, err)
		return nil
	}
	if !admin {
		return nil
	}
	return tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("Open dashboard").WithWebApp(&telego.WebAppInfo{URL: s.cfg.PublicBaseURL}),
		),
	)
}

func (s *Service) eventMarkup(ctx context.Context, event db.Event, chatID int64) telego.ReplyMarkup {
	rows := make([][]telego.InlineKeyboardButton, 0, 2)
	if url := s.messageURL(ctx, event); url != "" {
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("Open message").WithURL(url),
		))
	}
	admin, err := s.isAdminChat(ctx, chatID)
	if err != nil {
		log.Printf("check event action admin chat %d failed: %v", chatID, err)
	}
	if err == nil && admin && event.ID > 0 && strings.TrimSpace(event.Keyword) != "" {
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("Stop keyword").WithCallbackData(fmt.Sprintf("kwdel:%d", event.ID)),
		))
	}
	if len(rows) == 0 {
		return nil
	}
	return tu.InlineKeyboard(rows...)
}

func adminOnlyCommand(command string) bool {
	switch command {
	case "add", "watch", "del", "delete", "keywords", "recent", "login", "loginstatus", "cancel":
		return true
	default:
		return false
	}
}

func callbackChatID(query *telego.CallbackQuery) int64 {
	if query == nil {
		return 0
	}
	if query.Message != nil {
		if chatID := query.Message.GetChat().ID; chatID != 0 {
			return chatID
		}
	}
	return query.From.ID
}

func (s *Service) canSubscribe(ctx context.Context, chatID int64) (bool, error) {
	if len(s.cfg.BotAdminChatIDs) > 0 {
		return s.cfg.IsConfiguredBotAdmin(chatID), nil
	}
	first, ok, err := s.store.FirstSubscriber(ctx)
	if err != nil || !ok {
		return !ok, err
	}
	return first.ChatID == chatID, nil
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
	if event.MatchReason != "" {
		fmt.Fprintf(&b, "Why: %s (score %d)\n", event.MatchReason, event.MatchScore)
	}
	if event.RuleNote != "" {
		fmt.Fprintf(&b, "Specs: %s\n", event.RuleNote)
	}
	if !event.MessageDate.IsZero() {
		fmt.Fprintf(&b, "Time: %s\n", event.MessageDate.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "\n%s", truncate(event.Text, 2600))
	return b.String()
}

func formatDeletedMessages(deletion db.PrivateMessageDeletion) string {
	count := len(deletion.Messages)
	label := privateDialogLabel(deletion.Dialog)
	var b strings.Builder
	if count == 1 {
		fmt.Fprintf(&b, "🫥 В личном чате %s исчезло сообщение.\n", label)
	} else {
		fmt.Fprintf(&b, "🧹 В личном чате %s исчезли %d сообщений.\n", label, count)
		b.WriteString("Это может быть очистка истории, но Telegram не даёт отдельного признака «удалён весь чат».\n")
	}
	b.WriteString("Telegram не сообщает причину и автора удаления: это мог быть собеседник, другая твоя сессия или автоудаление.\n")

	shown := 0
	for _, message := range deletion.Messages {
		if shown == 6 {
			break
		}
		direction := "входящее"
		if message.Outgoing {
			direction = "исходящее"
		}
		content := strings.TrimSpace(message.Text)
		if content == "" {
			content = deletedMediaLabel(message.MediaType)
		}
		fmt.Fprintf(&b, "\n%d. %s · %s\n%s\n", shown+1, direction, message.MessageDate.Format(time.RFC3339), truncateUTF16(content, 480))
		shown++
	}
	if remaining := count - shown; remaining > 0 {
		fmt.Fprintf(&b, "\n…и ещё %d.", remaining)
	}
	return truncateUTF16(b.String(), 3900)
}

func privateDialogLabel(dialog db.PrivateDialog) string {
	title := strings.TrimSpace(dialog.Title)
	username := telegramUsername(dialog.Username)
	if title == "" && username != "" {
		return "@" + username
	}
	if title == "" {
		return fmt.Sprintf("user:%d", dialog.PeerID)
	}
	if username != "" && !strings.EqualFold(title, username) && !strings.EqualFold(title, "@"+username) {
		return fmt.Sprintf("%s (@%s)", title, username)
	}
	return title
}

func deletedMediaLabel(mediaType string) string {
	switch strings.TrimSpace(mediaType) {
	case "photo":
		return "[фото без подписи]"
	case "file":
		return "[файл без подписи]"
	case "contact":
		return "[контакт]"
	case "location":
		return "[геолокация]"
	case "poll":
		return "[опрос]"
	case "dice":
		return "[дайс]"
	case "game":
		return "[игра]"
	case "media":
		return "[медиа без подписи]"
	default:
		return "[сообщение без текста]"
	}
}

func helpText() string {
	return strings.Join([]string{
		"tg-radar commands:",
		"/start - subscribe this chat to alerts",
		"/stop - unsubscribe this chat",
		"/add keyword - add a radar phrase",
		"/watch name :: any1, any2 :: required1 :: excluded1 - add a flexible rule",
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

func truncateUTF16(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 3 {
		return ""
	}
	encoded := utf16.Encode([]rune(value))
	if len(encoded) <= limit {
		return value
	}
	encoded = encoded[:limit-3]
	if len(encoded) > 0 && encoded[len(encoded)-1] >= 0xD800 && encoded[len(encoded)-1] <= 0xDBFF {
		encoded = encoded[:len(encoded)-1]
	}
	return string(utf16.Decode(encoded)) + "..."
}
