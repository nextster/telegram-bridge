package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/nextster/tg-radar/internal/monitor"
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

func (m *loginManager) Start(ctx context.Context, message *telego.Message, payload string) error {
	if message.Chat.Type != "private" {
		return m.service.reply(ctx, message.Chat.ID, "Use /login in a private chat with this bot.", nil)
	}
	if m.service.monitorService == nil {
		return m.service.reply(ctx, message.Chat.ID, "Telegram user API is not configured.", nil)
	}

	admin, err := m.service.isAdminChat(ctx, message.Chat.ID)
	if err != nil {
		return err
	}
	if !admin {
		return m.service.reply(ctx, message.Chat.ID, "This chat is not allowed to run /login. Send /start from the admin chat first or set TG_RADAR_ADMIN_CHAT_IDS.", nil)
	}

	if status := m.service.monitorService.Status(); status.Authorized {
		if status.UserID != 0 {
			return m.service.reply(ctx, message.Chat.ID, fmt.Sprintf("Authorized: user_id=%d.", status.UserID), nil)
		}
		return m.service.reply(ctx, message.Chat.ID, "Authorized.", nil)
	}

	if phone := normalizePhone(payload); phone != "" {
		if err := m.service.store.SaveLoginPhone(ctx, message.Chat.ID, phone); err != nil {
			return err
		}
		return m.sendLoginLink(ctx, message.Chat.ID, phone)
	}

	if phone, ok, err := m.service.store.LoginPhone(ctx, message.Chat.ID); err != nil {
		return err
	} else if ok {
		return m.sendLoginLink(ctx, message.Chat.ID, phone)
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
		return m.service.reply(ctx, message.Chat.ID, "Login already running. /cancel", nil)
	}
	m.sessions[message.Chat.ID] = session
	m.mu.Unlock()

	go m.expirePhonePrompt(loginCtx, session)
	return m.sendPrompt(ctx, session, "Share phone", phoneKeyboard())
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
	if strings.HasPrefix(text, "/") {
		return true, m.service.reply(ctx, message.Chat.ID, "Send value or /cancel.", nil)
	}
	if text == "" {
		return true, m.service.reply(ctx, message.Chat.ID, "Send value or /cancel.", nil)
	}

	m.deleteIncoming(ctx, message)
	return true, m.finishPhone(ctx, session, text)
}

func (m *loginManager) Cancel(ctx context.Context, chatID int64) error {
	session := m.takeSession(chatID)
	if session == nil {
		return m.service.reply(ctx, chatID, "No login is in progress.", nil)
	}
	session.cancel()
	m.deletePrompt(ctx, session)
	return m.service.reply(ctx, chatID, "Cancelled.", removeKeyboard())
}

func (m *loginManager) handleContact(ctx context.Context, session *loginSession, message *telego.Message) error {
	if session.promptKind() != monitor.LoginPromptPhone {
		return m.service.reply(ctx, message.Chat.ID, "Send value or /cancel.", removeKeyboard())
	}
	if message.Contact.UserID != 0 && message.From != nil && message.Contact.UserID != message.From.ID {
		return m.service.reply(ctx, message.Chat.ID, "Share your own phone.", phoneKeyboard())
	}

	phone := strings.TrimSpace(message.Contact.PhoneNumber)
	if phone == "" {
		return m.service.reply(ctx, message.Chat.ID, "Empty phone. Type it manually.", phoneKeyboard())
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
		return m.service.reply(ctx, session.chatID, "Empty phone. Share phone or type it manually.", phoneKeyboard())
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
		return m.service.reply(ctx, chatID, "Set TG_RADAR_PUBLIC_URL to use site login.", removeKeyboard())
	}
	token, err := m.service.store.CreateLoginToken(ctx, chatID, normalizePhone(phone), loginLinkTTL)
	if err != nil {
		return err
	}
	loginURL := m.service.cfg.PublicBaseURL + "/login?token=" + url.QueryEscape(token.Token)
	return m.service.reply(ctx, chatID, "Open login. Enter code and 2FA on the site.", loginLinkMarkup(loginURL))
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
	m.finalReply(session, "Timed out. /login")
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

func (s *Service) isAdminChat(ctx context.Context, chatID int64) (bool, error) {
	if len(s.cfg.BotAdminChatIDs) > 0 {
		return s.cfg.IsConfiguredBotAdmin(chatID), nil
	}

	first, ok, err := s.store.FirstSubscriber(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return first.ChatID == chatID, nil
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
		tu.KeyboardRow(tu.KeyboardButton("Share phone").WithRequestContact()),
	).
		WithResizeKeyboard().
		WithOneTimeKeyboard().
		WithInputFieldPlaceholder("Share phone or type +995...")
}

func removeKeyboard() telego.ReplyMarkup {
	return tu.ReplyKeyboardRemove()
}

func loginLinkMarkup(loginURL string) telego.ReplyMarkup {
	return tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("Open login").WithURL(loginURL),
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
