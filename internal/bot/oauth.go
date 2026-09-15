package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const (
	oauthStartPrefix          = "oauth_"
	oauthCallbackPrefix       = "oauth:"
	oauthRevokeCallbackPrefix = "oauthrevoke:"
	oauthAllowChoice          = "allow"
	oauthDenyChoice           = "deny"
)

// botIdentity caches the bot username used in approval deep links.
type botIdentity struct {
	mu       sync.Mutex
	username string
}

func (s *Service) botUsername(ctx context.Context) (string, error) {
	s.identity.mu.Lock()
	defer s.identity.mu.Unlock()
	if s.identity.username != "" {
		return s.identity.username, nil
	}
	me, err := s.bot.GetMe(ctx)
	if err != nil {
		return "", fmt.Errorf("get bot identity: %w", err)
	}
	if me.Username == "" {
		return "", errors.New("bot has no username")
	}
	s.identity.username = me.Username
	return me.Username, nil
}

// OAuthApprovalLink returns a deep link that makes the user's Telegram client
// send /start oauth_<request>, in case the pushed prompt did not arrive.
func (s *Service) OAuthApprovalLink(ctx context.Context, requestID string) (string, error) {
	username, err := s.botUsername(ctx)
	if err != nil {
		return "", err
	}
	return "https://t.me/" + username + "?start=" + oauthStartPrefix + requestID, nil
}

// OAuthConnectionRevoked notifies a user that one of their connections was
// revoked.
func (s *Service) OAuthConnectionRevoked(ctx context.Context, userID int64, clientName, reason string) error {
	if userID <= 0 {
		return errors.New("connection owner is unknown")
	}
	return s.sendHTML(ctx, userID, fmt.Sprintf("⚠️ MCP-подключение %s отключено: %s.\nЕсли это были не вы, проверьте устройства и /connections.",
		codeHTML(clientName), html.EscapeString(reason)), nil)
}

// OAuthConnectionCreated tells a user that a new client has access to their
// account.
func (s *Service) OAuthConnectionCreated(ctx context.Context, userID int64, clientName, clientIP string) error {
	if userID <= 0 {
		return errors.New("connection owner is unknown")
	}
	return s.sendHTML(ctx, userID, fmt.Sprintf("🔗 Новое MCP-подключение к вашему Telegram: %s, IP %s.\nЕсли подключали не вы, сразу отключите его в /connections.",
		codeHTML(clientName), codeHTML(clientIP)), nil)
}

// canApproveOAuth requires a private chat and a connected Telegram account.
// The approval grants access to that user's own account only.
func (s *Service) canApproveOAuth(ctx context.Context, chatID, userID int64) (bool, error) {
	if userID <= 0 || chatID != userID {
		return false, nil
	}
	return s.connected(ctx, userID)
}

const oauthForeignRequestText = "⚠️ Этот запрос на подключение открыт под другим аккаунтом Telegram. Если ссылку вам кто-то прислал, он пытается получить доступ к вашей переписке. Ничего не подтверждайте."

// OAuthApprovalRequested sends the approval prompt to the user who signed in
// with Telegram for the request.
func (s *Service) OAuthApprovalRequested(ctx context.Context, userID int64, requestID string) error {
	return s.sendOAuthPrompt(ctx, userID, requestID)
}

func (s *Service) handleOAuthStart(ctx context.Context, message *telego.Message, requestID string) error {
	userID, private := privateSender(message)
	if !private {
		return s.reply(ctx, message.Chat.ID, "Подтверждать подключения можно только в личном чате с ботом.", nil)
	}
	return s.sendOAuthPrompt(ctx, userID, requestID)
}

func (s *Service) sendOAuthPrompt(ctx context.Context, userID int64, requestID string) error {
	allowed, err := s.canApproveOAuth(ctx, userID, userID)
	if err != nil {
		return err
	}
	if !allowed {
		return s.reply(ctx, userID, "Сначала подключите свой Telegram: /login. Доступ получит только ваш аккаунт.", nil)
	}
	request, found, err := s.store.GetOAuthRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if !found || request.Status != "pending" || !time.Now().Before(request.ExpiresAt) {
		return s.reply(ctx, userID, "Запрос на подключение устарел. Запустите подключение заново.", nil)
	}
	if request.BoundUserID != userID {
		return s.reply(ctx, userID, oauthForeignRequestText, nil)
	}
	clientName := "MCP-клиент"
	if client, ok, err := s.store.GetOAuthClient(ctx, request.ClientID); err == nil && ok && client.Name != "" {
		clientName = client.Name
	}
	markup := tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("✅ Разрешить").WithCallbackData(oauthCallbackPrefix+request.ID+":"+oauthAllowChoice),
		tu.InlineKeyboardButton("Отклонить").WithCallbackData(oauthCallbackPrefix+request.ID+":"+oauthDenyChoice),
	))
	text := fmt.Sprintf("🔐 <b>Доступ к вашему Telegram через MCP</b>\n\nКлиент: %s (название задаёт сам клиент)\nIP: %s\nБраузер: %s\n\nВы вошли через Telegram на странице подключения. Разрешите доступ, только если подключение запускали вы.",
		codeHTML(clientName), codeHTML(request.ClientIP), codeHTML(request.UserAgent))
	return s.sendHTML(ctx, userID, text, markup)
}

func (s *Service) handleOAuthCallback(ctx context.Context, query *telego.CallbackQuery, userID int64, payload string) error {
	allowed, err := s.canApproveOAuth(ctx, callbackChatID(query), userID)
	if err != nil {
		return err
	}
	if !allowed {
		return s.answerCallback(ctx, query.ID, "Сначала подключите свой Telegram: /login.")
	}
	requestID, choice, ok := strings.Cut(payload, ":")
	if !ok || requestID == "" || (choice != oauthAllowChoice && choice != oauthDenyChoice) {
		return s.answerCallback(ctx, query.ID, "Кнопка устарела. Запустите подключение заново.")
	}
	request, found, err := s.store.GetOAuthRequest(ctx, requestID)
	if err != nil {
		_ = s.answerCallback(ctx, query.ID, "Не удалось проверить запрос.")
		return err
	}
	if !found {
		s.finishOAuthPrompt(ctx, query, "⌛ Запрос на подключение устарел.")
		return s.answerCallback(ctx, query.ID, "Запрос устарел.")
	}
	if request.BoundUserID != userID {
		s.finishOAuthPrompt(ctx, query, oauthForeignRequestText)
		return s.answerCallback(ctx, query.ID, "Запрос открыт под другим аккаунтом.")
	}
	approve := choice == oauthAllowChoice
	_, changed, err := s.store.DecideOAuthRequest(ctx, request.ID, approve, userID, time.Now())
	if err != nil {
		_ = s.answerCallback(ctx, query.ID, "Не удалось сохранить решение.")
		return err
	}
	if !changed {
		s.finishOAuthPrompt(ctx, query, "⌛ Запрос уже обработан или устарел.")
		return s.answerCallback(ctx, query.ID, "Запрос уже обработан или устарел.")
	}
	if approve {
		s.finishOAuthPrompt(ctx, query, "✅ Доступ разрешён. Вернитесь в браузер. Отключить можно через /connections.")
		return s.answerCallback(ctx, query.ID, "Доступ разрешён.")
	}
	s.finishOAuthPrompt(ctx, query, "⛔ Подключение отклонено.")
	return s.answerCallback(ctx, query.ID, "Отклонено.")
}

func (s *Service) finishOAuthPrompt(ctx context.Context, query *telego.CallbackQuery, text string) {
	if query.Message == nil {
		return
	}
	chat := query.Message.GetChat()
	messageID := query.Message.GetMessageID()
	if chat.ID == 0 || messageID == 0 {
		return
	}
	if _, err := s.bot.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:    telego.ChatID{ID: chat.ID},
		MessageID: messageID,
		Text:      text,
	}); err != nil {
		log.Printf("edit OAuth approval message failed: %v", err)
		s.clearCallbackMarkup(ctx, query)
	}
}

func (s *Service) handleConnections(ctx context.Context, message *telego.Message) error {
	userID, private := privateSender(message)
	if !private {
		return s.reply(ctx, message.Chat.ID, "MCP-подключения видны только в личном чате с ботом.", nil)
	}
	grants, err := s.store.ListOAuthGrants(ctx, userID)
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		return s.reply(ctx, message.Chat.ID, "Нет активных MCP-подключений.", nil)
	}
	lines := []string{"<b>MCP-подключения</b>"}
	rows := make([][]telego.InlineKeyboardButton, 0, len(grants))
	for index, grant := range grants {
		lines = append(lines, fmt.Sprintf("%d. %s — с %s, активность %s", index+1, codeHTML(grant.ClientName),
			grant.CreatedAt.Format("2006-01-02"), grant.LastUsedAt.Format("2006-01-02 15:04 UTC")))
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton(fmt.Sprintf("Отключить %d", index+1)).WithCallbackData(oauthRevokeCallbackPrefix+grant.ID),
		))
	}
	return s.sendHTML(ctx, message.Chat.ID, strings.Join(lines, "\n"), tu.InlineKeyboard(rows...))
}

func (s *Service) handleOAuthRevokeCallback(ctx context.Context, query *telego.CallbackQuery, userID int64, grantID string) error {
	grant, revoked, err := s.store.RevokeOAuthGrant(ctx, userID, grantID, time.Now())
	if err != nil {
		_ = s.answerCallback(ctx, query.ID, "Не удалось отключить.")
		return err
	}
	if !revoked {
		return s.answerCallback(ctx, query.ID, "Подключение не найдено или уже отключено.")
	}
	if err := s.answerCallback(ctx, query.ID, "Отключено."); err != nil {
		log.Printf("answer OAuth revoke callback failed: %v", err)
	}
	return s.sendHTML(ctx, userID, "Отключено MCP-подключение "+codeHTML(grant.ClientName)+".", nil)
}

func (s *Service) sendHTML(ctx context.Context, chatID int64, text string, markup telego.ReplyMarkup) error {
	_, err := s.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:      telego.ChatID{ID: chatID},
		Text:        text,
		ParseMode:   telego.ModeHTML,
		ReplyMarkup: markup,
	})
	return err
}

// codeHTML renders untrusted text, such as a client-chosen name, as inline
// code so Telegram does not turn commands, mentions, or URLs into links.
func codeHTML(value string) string {
	if strings.TrimSpace(value) == "" {
		value = "—"
	}
	return "<code>" + html.EscapeString(value) + "</code>"
}
