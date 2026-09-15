package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
	"github.com/nextster/telegram-bridge/internal/monitor"
)

// Accounts is the part of the account manager the bot needs. Every call is
// scoped to one Telegram user.
type Accounts interface {
	Status(owner int64) monitor.Status
	Connected(ctx context.Context, owner int64) (bool, error)
	Logout(ctx context.Context, owner int64) error
}

// Service is the bot. Every command acts only for the Telegram user who sent
// it, in that user's private chat with the bot.
type Service struct {
	bot      *telego.Bot
	store    *db.Store
	cfg      config.Config
	accounts Accounts
	login    *loginManager
	identity botIdentity
}

func New(cfg config.Config, store *db.Store) (*Service, error) {
	if strings.TrimSpace(cfg.BotToken) == "" {
		return nil, errors.New("bot token is empty")
	}
	b, err := telego.NewBot(cfg.BotToken, telego.WithAPICaller(&telegoapi.RetryCaller{
		Caller:       telegoapi.HTTPCaller{Client: &http.Client{}},
		MaxAttempts:  5,
		ExponentBase: 2,
		StartDelay:   time.Second,
		MaxDelay:     2 * time.Minute,
		RateLimit:    telegoapi.RetryRateLimitWait,
	}))
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}
	service := &Service{bot: b, store: store, cfg: cfg}
	service.login = newLoginManager(service)
	return service, nil
}

func (s *Service) SetAccounts(accounts Accounts) {
	s.accounts = accounts
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

// NotifyEvent alerts the owner of the matched rule, in their private chat, if
// they have alerts enabled.
func (s *Service) NotifyEvent(ctx context.Context, event db.Event) error {
	subscribed, err := s.alertsEnabled(ctx, event.OwnerUserID)
	if err != nil || !subscribed {
		return err
	}
	_, err = s.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:      telego.ChatID{ID: event.OwnerUserID},
		Text:        formatEvent(event),
		ReplyMarkup: s.eventMarkup(ctx, event),
	})
	if err != nil {
		return fmt.Errorf("send alert to %d: %w", event.OwnerUserID, err)
	}
	return nil
}

// NotifyDeletedMessages sends an archive alert only to the account owner.
func (s *Service) NotifyDeletedMessages(ctx context.Context, deletion db.PrivateMessageDeletion) error {
	owner := deletion.Dialog.OwnerUserID
	if len(deletion.Messages) == 0 {
		return nil
	}
	subscribed, err := s.alertsEnabled(ctx, owner)
	if err != nil {
		return err
	}
	if !subscribed {
		return fmt.Errorf("private deletion alert owner %d has not subscribed to the bot", owner)
	}
	_, err = s.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: owner},
		Text:   formatDeletedMessages(deletion),
	})
	if err != nil {
		return fmt.Errorf("send deletion alert to owner chat %d: %w", owner, err)
	}
	return nil
}

// NotifyUser sends a system message to one user's private chat.
func (s *Service) NotifyUser(ctx context.Context, userID int64, text string) error {
	text = strings.TrimSpace(text)
	if userID <= 0 || text == "" {
		return nil
	}
	return s.reply(ctx, userID, text, nil)
}

// alertsEnabled reports whether a user started the bot and did not /stop. In
// a private Bot API chat the chat ID equals the user ID.
func (s *Service) alertsEnabled(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	return s.store.IsSubscribed(ctx, userID)
}

func (s *Service) setCommands(ctx context.Context) error {
	return s.bot.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "start", Description: "включить уведомления"},
			{Command: "stop", Description: "выключить уведомления"},
			{Command: "login", Description: "подключить свой Telegram"},
			{Command: "loginstatus", Description: "статус подключения"},
			{Command: "logout", Description: "отключить свой Telegram"},
			{Command: "add", Description: "добавить ключевое слово"},
			{Command: "watch", Description: "добавить гибкое правило"},
			{Command: "del", Description: "удалить правило"},
			{Command: "keywords", Description: "мои правила"},
			{Command: "recent", Description: "последние совпадения"},
			{Command: "connections", Description: "MCP-подключения"},
			{Command: "cancel", Description: "отменить вход"},
			{Command: "help", Description: "команды"},
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
	userID, private := privateSender(message)
	if !private {
		if strings.HasPrefix(text, "/") {
			return s.reply(ctx, message.Chat.ID, "Бот работает только в личном чате.", nil)
		}
		return nil
	}
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
		if requestID, ok := strings.CutPrefix(strings.TrimSpace(payload), oauthStartPrefix); ok {
			return s.handleOAuthStart(ctx, message, requestID)
		}
		return s.handleStart(ctx, message, userID)
	case "stop":
		return s.handleStop(ctx, userID)
	case "add":
		return s.handleAddKeyword(ctx, userID, payload)
	case "watch":
		return s.handleAddWatchRule(ctx, userID, payload)
	case "del", "delete":
		return s.handleDeleteKeyword(ctx, userID, payload)
	case "keywords":
		return s.handleKeywords(ctx, userID)
	case "recent":
		return s.handleRecent(ctx, userID)
	case "login":
		return s.login.Start(ctx, message)
	case "loginstatus":
		return s.handleLoginStatus(ctx, userID)
	case "logout":
		return s.handleLogout(ctx, userID)
	case "cancel":
		return s.login.Cancel(ctx, userID)
	case "connections":
		return s.handleConnections(ctx, message)
	case "help":
		return s.reply(ctx, userID, helpText(), nil)
	default:
		return s.reply(ctx, userID, "Неизвестная команда. Отправьте /help.", nil)
	}
}

// privateSender returns the sender of a message in their private chat with the
// bot. Group chats and anonymous senders are rejected.
func privateSender(message *telego.Message) (int64, bool) {
	if message == nil || message.From == nil || message.Chat.Type != "private" {
		return 0, false
	}
	if message.From.ID <= 0 || message.Chat.ID != message.From.ID {
		return 0, false
	}
	return message.From.ID, true
}

// callbackUser returns the user who pressed a button in their private chat.
func callbackUser(query *telego.CallbackQuery) (int64, bool) {
	if query == nil || query.From.ID <= 0 {
		return 0, false
	}
	if callbackChatID(query) != query.From.ID {
		return 0, false
	}
	return query.From.ID, true
}

func (s *Service) handleCallbackQuery(ctx context.Context, query *telego.CallbackQuery) error {
	if query == nil {
		return nil
	}
	data := strings.TrimSpace(query.Data)
	userID, private := callbackUser(query)
	if !private {
		return s.answerCallback(ctx, query.ID, "Кнопки работают только в личном чате с ботом.")
	}
	if payload, ok := strings.CutPrefix(data, oauthCallbackPrefix); ok {
		return s.handleOAuthCallback(ctx, query, userID, payload)
	}
	if grantID, ok := strings.CutPrefix(data, oauthRevokeCallbackPrefix); ok {
		return s.handleOAuthRevokeCallback(ctx, query, userID, grantID)
	}
	if rawEventID, ok := strings.CutPrefix(data, "kwdel:"); ok {
		return s.handleStopKeywordCallback(ctx, query, userID, rawEventID)
	}
	return s.answerCallback(ctx, query.ID, "Неизвестное действие.")
}

func (s *Service) handleStopKeywordCallback(ctx context.Context, query *telego.CallbackQuery, userID int64, rawEventID string) error {
	eventID, err := strconv.ParseInt(strings.TrimSpace(rawEventID), 10, 64)
	if err != nil || eventID <= 0 {
		return s.answerCallback(ctx, query.ID, "This button is stale.")
	}

	event, ok, err := s.store.GetEvent(ctx, userID, eventID)
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
	rows, err := s.store.DeleteKeyword(ctx, userID, deleteValue)
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

func (s *Service) handleStart(ctx context.Context, message *telego.Message, userID int64) error {
	if err := s.store.UpsertSubscriber(ctx, db.Subscriber{
		ChatID:    userID,
		Username:  message.From.Username,
		FirstName: message.From.FirstName,
		LastName:  message.From.LastName,
	}); err != nil {
		return err
	}
	stats, err := s.store.Stats(ctx, userID)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Уведомления включены.\n\nПравил: %d\nСовпадений: %d", stats.Keywords, stats.Events)
	if connected, err := s.connected(ctx, userID); err == nil && !connected {
		text += "\n\nЧтобы бот видел ваши чаты и работал MCP, подключите свой Telegram: /login"
	}
	return s.reply(ctx, userID, text, s.webAppMarkup())
}

func (s *Service) connected(ctx context.Context, userID int64) (bool, error) {
	if s.accounts == nil {
		return false, nil
	}
	return s.accounts.Connected(ctx, userID)
}

func (s *Service) handleStop(ctx context.Context, userID int64) error {
	if err := s.store.DeleteSubscriber(ctx, userID); err != nil {
		return err
	}
	return s.reply(ctx, userID, "Уведомления выключены. Подключение Telegram и правила сохранены.", nil)
}

func (s *Service) handleAddKeyword(ctx context.Context, userID int64, payload string) error {
	keyword, err := s.store.AddKeyword(ctx, userID, payload)
	if err != nil {
		return s.reply(ctx, userID, "Использование: /add слово", nil)
	}
	return s.reply(ctx, userID, fmt.Sprintf("Добавлено правило #%d: %s", keyword.ID, keyword.Phrase), nil)
}

func (s *Service) handleAddWatchRule(ctx context.Context, userID int64, payload string) error {
	parts := strings.Split(payload, "::")
	if len(parts) < 2 {
		return s.reply(ctx, userID, "Использование: /watch название :: любое, синоним :: обязательные :: исключения", nil)
	}
	rule, err := s.store.UpsertWatchRule(ctx, userID, db.Keyword{
		Phrase:       strings.TrimSpace(parts[0]),
		AnyTerms:     splitRuleTerms(parts[1]),
		AllTerms:     splitRulePart(parts, 2),
		ExcludeTerms: splitRulePart(parts, 3),
		Enabled:      true,
	})
	if err != nil {
		return s.reply(ctx, userID, "Не удалось добавить правило: "+err.Error(), nil)
	}
	return s.reply(ctx, userID, fmt.Sprintf("Слежу #%d %s\nлюбое: %s", rule.ID, rule.Phrase, strings.Join(rule.AnyTerms, ", ")), nil)
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

func (s *Service) handleDeleteKeyword(ctx context.Context, userID int64, payload string) error {
	rows, err := s.store.DeleteKeyword(ctx, userID, payload)
	if err != nil || rows == 0 {
		return s.reply(ctx, userID, "Использование: /del слово-или-номер", nil)
	}
	return s.reply(ctx, userID, "Правило удалено.", nil)
}

func (s *Service) handleKeywords(ctx context.Context, userID int64) error {
	keywords, err := s.store.ListKeywords(ctx, userID)
	if err != nil {
		return err
	}
	if len(keywords) == 0 {
		return s.reply(ctx, userID, "Правил пока нет. Добавьте: /add слово", nil)
	}

	var b strings.Builder
	b.WriteString("Ваши правила:\n")
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
	return s.reply(ctx, userID, strings.TrimSpace(b.String()), s.webAppMarkup())
}

func formatRuleTerms(terms []string, limit int) string {
	if limit <= 0 || len(terms) <= limit {
		return strings.Join(terms, ", ")
	}
	return fmt.Sprintf("%s (+%d)", strings.Join(terms[:limit], ", "), len(terms)-limit)
}

func (s *Service) handleRecent(ctx context.Context, userID int64) error {
	events, err := s.store.ListEvents(ctx, userID, 5)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return s.reply(ctx, userID, "Совпадений пока нет.", nil)
	}

	var b strings.Builder
	b.WriteString("Последние совпадения:\n")
	for _, event := range events {
		fmt.Fprintf(&b, "\n#%d [%s] %s:%d\n%s", event.ID, event.Keyword, event.SourcePeerType, event.SourcePeerID, truncate(event.Text, 180))
	}
	return s.reply(ctx, userID, strings.TrimSpace(b.String()), s.webAppMarkup())
}

func (s *Service) handleLoginStatus(ctx context.Context, userID int64) error {
	if s.accounts == nil {
		return s.reply(ctx, userID, "Telegram API не настроен на сервере.", nil)
	}
	status := s.accounts.Status(userID)
	connected, err := s.accounts.Connected(ctx, userID)
	if err != nil {
		return err
	}
	switch {
	case !status.Configured:
		return s.reply(ctx, userID, "Telegram API не настроен на сервере.", nil)
	case status.Authorized:
		return s.reply(ctx, userID, "Ваш Telegram подключён и работает.", nil)
	case connected:
		return s.reply(ctx, userID, "Ваш Telegram подключён, сервер переподключается.", nil)
	default:
		return s.reply(ctx, userID, "Ваш Telegram не подключён. Отправьте /login.", nil)
	}
}

func (s *Service) handleLogout(ctx context.Context, userID int64) error {
	if s.accounts == nil {
		return s.reply(ctx, userID, "Telegram API не настроен на сервере.", nil)
	}
	connected, err := s.accounts.Connected(ctx, userID)
	if err != nil {
		return err
	}
	if !connected {
		return s.reply(ctx, userID, "Ваш Telegram не подключён.", nil)
	}
	if err := s.accounts.Logout(ctx, userID); err != nil {
		return err
	}
	return s.reply(ctx, userID, "Готово: сессия Telegram завершена, MCP-подключения и токены удалены. Правила сохранены.", nil)
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
			tu.InlineKeyboardButton("Открыть панель").WithWebApp(&telego.WebAppInfo{URL: s.cfg.PublicBaseURL}),
		),
	)
}

// eventMarkup is only ever attached to an alert in the event owner's chat.
func (s *Service) eventMarkup(ctx context.Context, event db.Event) telego.ReplyMarkup {
	rows := make([][]telego.InlineKeyboardButton, 0, 2)
	if url := s.messageURL(ctx, event); url != "" {
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("Open message").WithURL(url),
		))
	}
	actions := make([]telego.InlineKeyboardButton, 0, 2)
	if ruleURL := s.ruleWebAppURL(event.RuleID); ruleURL != "" {
		actions = append(actions, tu.InlineKeyboardButton("Правило").WithWebApp(&telego.WebAppInfo{URL: ruleURL}))
	}
	if event.ID > 0 && strings.TrimSpace(event.Keyword) != "" {
		actions = append(actions, tu.InlineKeyboardButton("Stop keyword").WithCallbackData(fmt.Sprintf("kwdel:%d", event.ID)))
	}
	if len(actions) > 0 {
		rows = append(rows, tu.InlineKeyboardRow(actions...))
	}
	if len(rows) == 0 {
		return nil
	}
	return tu.InlineKeyboard(rows...)
}

func (s *Service) ruleWebAppURL(ruleID int64) string {
	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.PublicBaseURL), "/")
	if baseURL == "" || ruleID <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/#rule-%d", baseURL, ruleID)
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

func (s *Service) messageURL(ctx context.Context, event db.Event) string {
	peer, ok, err := s.store.GetMonitorPeer(ctx, event.OwnerUserID, event.SourcePeerType, event.SourcePeerID)
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
	content := strings.TrimSpace(event.Text)
	if content == "" {
		content = "[сообщение без текста]"
	}
	return "🔎 Найдено:\n\n" + truncate(content, 2800)
}

func formatDeletedMessages(deletion db.PrivateMessageDeletion) string {
	count := len(deletion.Messages)
	label := privateDialogLabel(deletion.Dialog)
	var b strings.Builder
	if count == 1 {
		fmt.Fprintf(&b, "🫥 Удалено из: %s\n", label)
	} else {
		fmt.Fprintf(&b, "🫥 Удалено из: %s · %d сообщений\n", label, count)
	}

	shown := 0
	for _, message := range deletion.Messages {
		if shown == 6 {
			break
		}
		content := strings.TrimSpace(message.Text)
		if content == "" {
			content = deletedMediaLabel(message.MediaType)
		}
		if count == 1 {
			fmt.Fprintf(&b, "\n%s", truncateUTF16(content, 1200))
		} else {
			fmt.Fprintf(&b, "\n%d. %s", shown+1, truncateUTF16(content, 480))
		}
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
		"Команды telegram-bridge:",
		"/login — подключить свой Telegram",
		"/loginstatus — статус подключения",
		"/logout — отключить свой Telegram, MCP-подключения и токены",
		"/start — включить уведомления",
		"/stop — выключить уведомления",
		"/add слово — добавить правило",
		"/watch название :: любое1, любое2 :: обязательное :: исключение — гибкое правило",
		"/del слово-или-номер — удалить правило",
		"/keywords — мои правила",
		"/recent — последние совпадения",
		"/connections — MCP-подключения",
		"/cancel — отменить вход",
		"",
		"Всё в боте и на панели относится только к вашему аккаунту.",
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
