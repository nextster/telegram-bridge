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

const oauthRevokeCallbackPrefix = "oauthrevoke:"

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

// OAuthBotLink returns a link that opens the bot.
func (s *Service) OAuthBotLink(ctx context.Context) (string, error) {
	username, err := s.botUsername(ctx)
	if err != nil {
		return "", err
	}
	return "https://t.me/" + username, nil
}

// OAuthAccountConnected reports whether the user has connected their own
// Telegram account, which MCP clients then act on.
func (s *Service) OAuthAccountConnected(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	return s.connected(ctx, userID)
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
// account, with a button that cuts it off.
func (s *Service) OAuthConnectionCreated(ctx context.Context, userID int64, grantID, clientName, clientIP string) error {
	if userID <= 0 {
		return errors.New("connection owner is unknown")
	}
	markup := tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("Отключить").WithCallbackData(oauthRevokeCallbackPrefix + grantID),
	))
	return s.sendHTML(ctx, userID, fmt.Sprintf("🔗 Новое MCP-подключение к вашему Telegram: %s, IP %s.\nЕсли подключали не вы, нажмите «Отключить».",
		codeHTML(clientName), codeHTML(clientIP)), markup)
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
