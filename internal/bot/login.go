package bot

import (
	"context"
	"errors"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/nextster/telegram-bridge/internal/monitor"
)

const (
	loginTimeout = 10 * time.Minute
	loginLinkTTL = 15 * time.Minute
)

type loginManager struct {
	service *Service

	mu       sync.Mutex
	sessions map[int64]*loginSession
}

type loginSession struct {
	chatID int64
	cancel context.CancelFunc

	mu              sync.Mutex
	want            monitor.LoginPromptKind
	phone           string
	promptMessageID int
}

func newLoginManager(service *Service) *loginManager {
	return &loginManager{
		service:  service,
		sessions: make(map[int64]*loginSession),
	}
}

// Start begins a login for the sender's own account. The phone number must
// come from Telegram's contact button, which proves it belongs to the sender,
// so the bot cannot be used to send login codes to other people's phones.
func (m *loginManager) Start(ctx context.Context, message *telego.Message) error {
	userID, private := privateSender(message)
	if !private {
		return m.service.reply(ctx, message.Chat.ID, "Используйте /login в личном чате с ботом.", nil)
	}
	if m.service.accounts == nil {
		return m.service.reply(ctx, userID, "Telegram API не настроен на сервере.", nil)
	}
	connected, err := m.service.accounts.Connected(ctx, userID)
	if err != nil {
		return err
	}
	if connected {
		return m.service.reply(ctx, userID, "Ваш Telegram уже подключён. Чтобы подключить заново, сначала отправьте /logout.", nil)
	}

	if phone, ok, err := m.service.store.LoginPhone(ctx, userID); err != nil {
		return err
	} else if ok {
		return m.sendLoginLink(ctx, userID, phone)
	}

	loginCtx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	session := &loginSession{
		chatID: message.Chat.ID,
		cancel: cancel,
		want:   monitor.LoginPromptPhone,
	}

	m.mu.Lock()
	if _, exists := m.sessions[message.Chat.ID]; exists {
		m.mu.Unlock()
		cancel()
		return m.service.reply(ctx, message.Chat.ID, "Вход уже начат. /cancel — отменить.", nil)
	}
	m.sessions[message.Chat.ID] = session
	m.mu.Unlock()

	go m.expirePhonePrompt(loginCtx, session)
	return m.sendPrompt(ctx, session, "Нажмите «Поделиться номером» — так Telegram подтвердит, что номер ваш.", phoneKeyboard())
}

func (m *loginManager) HandleMessage(ctx context.Context, message *telego.Message) (bool, error) {
	session := m.session(message.Chat.ID)
	if session == nil {
		return false, nil
	}

	if message.Contact != nil {
		return true, m.handleContact(ctx, session, message)
	}

	text := strings.TrimSpace(message.Text)
	if strings.EqualFold(firstField(text), "/cancel") {
		return true, m.Cancel(ctx, message.Chat.ID)
	}
	if text != "" && !strings.HasPrefix(text, "/") {
		m.deleteIncoming(ctx, message)
	}
	return true, m.service.reply(ctx, message.Chat.ID, "Номер нужно отправить кнопкой «Поделиться номером». /cancel — отменить.", phoneKeyboard())
}

func (m *loginManager) Cancel(ctx context.Context, chatID int64) error {
	session := m.takeSession(chatID)
	if session == nil {
		return m.service.reply(ctx, chatID, "Вход не начат.", nil)
	}
	session.cancel()
	m.deletePrompt(ctx, session)
	return m.service.reply(ctx, chatID, "Вход отменён.", removeKeyboard())
}

func (m *loginManager) handleContact(ctx context.Context, session *loginSession, message *telego.Message) error {
	if session.promptKind() != monitor.LoginPromptPhone {
		return m.service.reply(ctx, message.Chat.ID, "/cancel — отменить вход.", removeKeyboard())
	}
	if message.From == nil || message.Contact.UserID == 0 || message.Contact.UserID != message.From.ID {
		return m.service.reply(ctx, message.Chat.ID, "Нужен ваш собственный номер — нажмите «Поделиться номером».", phoneKeyboard())
	}

	phone := strings.TrimSpace(message.Contact.PhoneNumber)
	if phone == "" {
		return m.service.reply(ctx, message.Chat.ID, "Telegram не передал номер. Попробуйте ещё раз.", phoneKeyboard())
	}

	m.deleteIncoming(ctx, message)
	return m.finishPhone(ctx, session, phone)
}

func (s *loginSession) setPromptKind(kind monitor.LoginPromptKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.want = kind
}

func (s *loginSession) promptKind() monitor.LoginPromptKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.want
}

func (s *loginSession) setPhone(phone string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phone = strings.TrimSpace(phone)
}

func (s *loginSession) loginPhone() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.phone)
}

func (s *loginSession) setPromptMessageID(messageID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptMessageID = messageID
}

func (s *loginSession) takePromptMessageID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	messageID := s.promptMessageID
	s.promptMessageID = 0
	return messageID
}

func (m *loginManager) finishPhone(ctx context.Context, session *loginSession, phone string) error {
	phone = normalizePhone(phone)
	if phone == "" {
		return m.service.reply(ctx, session.chatID, "Telegram не передал номер. Попробуйте ещё раз.", phoneKeyboard())
	}
	session.setPhone(phone)
	if err := m.service.store.SaveLoginPhone(ctx, session.chatID, phone); err != nil {
		return err
	}
	session.cancel()
	m.removeSession(session)
	m.deletePrompt(ctx, session)
	return m.sendLoginLink(ctx, session.chatID, phone)
}

func (m *loginManager) sendLoginLink(ctx context.Context, chatID int64, phone string) error {
	if m.service.cfg.PublicBaseURL == "" {
		return m.service.reply(ctx, chatID, "На сервере не задан TELEGRAM_BRIDGE_PUBLIC_URL.", removeKeyboard())
	}
	token, err := m.service.store.CreateLoginToken(ctx, chatID, normalizePhone(phone), loginLinkTTL)
	if err != nil {
		return err
	}
	loginURL := m.service.cfg.PublicBaseURL + "/login?token=" + url.QueryEscape(token.Token)
	return m.service.reply(ctx, chatID, "Откройте страницу входа и введите там код из Telegram и пароль 2FA. Не отправляйте код в этот чат.", loginLinkMarkup(loginURL))
}

func (m *loginManager) expirePhonePrompt(ctx context.Context, session *loginSession) {
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return
	}
	m.mu.Lock()
	if current := m.sessions[session.chatID]; current != session {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, session.chatID)
	m.mu.Unlock()
	m.finalReply(session, "Время вышло. /login — начать заново.")
}

func (m *loginManager) session(chatID int64) *loginSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[chatID]
}

func (m *loginManager) takeSession(chatID int64) *loginSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[chatID]
	delete(m.sessions, chatID)
	return session
}

func (m *loginManager) removeSession(session *loginSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.sessions[session.chatID]; current == session {
		delete(m.sessions, session.chatID)
	}
}

func (m *loginManager) sendPrompt(ctx context.Context, session *loginSession, text string, markup telego.ReplyMarkup) error {
	m.deletePrompt(ctx, session)
	msg, err := m.service.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:      telego.ChatID{ID: session.chatID},
		Text:        text,
		ReplyMarkup: markup,
	})
	if err != nil {
		return err
	}
	if msg != nil {
		session.setPromptMessageID(msg.MessageID)
	}
	return nil
}

func (m *loginManager) finalReply(session *loginSession, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	m.deletePrompt(ctx, session)
	if err := m.service.reply(ctx, session.chatID, text, removeKeyboard()); err != nil {
		log.Printf("send login final reply to %d failed: %v", session.chatID, err)
	}
}

func (m *loginManager) deletePrompt(ctx context.Context, session *loginSession) {
	messageID := session.takePromptMessageID()
	if messageID == 0 {
		return
	}
	if err := m.service.bot.DeleteMessage(ctx, &telego.DeleteMessageParams{
		ChatID:    telego.ChatID{ID: session.chatID},
		MessageID: messageID,
	}); err != nil {
		log.Printf("delete login prompt %d in chat %d failed: %v", messageID, session.chatID, err)
	}
}

func (m *loginManager) replyBackground(chatID int64, text string) {
	m.replyBackgroundMarkup(chatID, text, nil)
}

func (m *loginManager) replyBackgroundMarkup(chatID int64, text string, markup telego.ReplyMarkup) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.service.reply(ctx, chatID, text, markup); err != nil {
		log.Printf("send login reply to %d failed: %v", chatID, err)
	}
}

func (m *loginManager) deleteIncoming(ctx context.Context, message *telego.Message) {
	if message.MessageID == 0 {
		return
	}
	if err := m.service.bot.DeleteMessage(ctx, &telego.DeleteMessageParams{
		ChatID:    telego.ChatID{ID: message.Chat.ID},
		MessageID: message.MessageID,
	}); err != nil {
		log.Printf("delete login message %d in chat %d failed: %v", message.MessageID, message.Chat.ID, err)
	}
}

func firstField(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func phoneKeyboard() telego.ReplyMarkup {
	return tu.Keyboard(
		tu.KeyboardRow(tu.KeyboardButton("Поделиться номером").WithRequestContact()),
	).
		WithResizeKeyboard().
		WithOneTimeKeyboard().
		WithInputFieldPlaceholder("Нажмите «Поделиться номером»")
}

func removeKeyboard() telego.ReplyMarkup {
	return tu.ReplyKeyboardRemove()
}

func loginLinkMarkup(loginURL string) telego.ReplyMarkup {
	return tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("Открыть вход").WithURL(loginURL),
		),
	)
}

func normalizePhone(phone string) string {
	phone = strings.TrimSpace(phone)
	phone = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(phone)
	if phone == "" || strings.HasPrefix(phone, "+") {
		return phone
	}
	if phone[0] >= '0' && phone[0] <= '9' {
		return "+" + phone
	}
	return phone
}
