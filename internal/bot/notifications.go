package bot

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

// Notifications use the existing bot identity but never the polling client's
// RetryCaller: retrying sendMessage after a timeout could create duplicates.
func (s *Service) notificationBot() (*telego.Bot, error) {
	return telego.NewBot(s.cfg.BotToken, telego.WithAPICaller(telegoapi.HTTPCaller{Client: &http.Client{Timeout: 15 * time.Second}}))
}

func (s *Service) CheckNotificationChat(ctx context.Context, chatID int64) error {
	b, err := s.notificationBot()
	if err != nil {
		return errors.New("notification bot is unavailable")
	}
	chat, err := b.GetChat(ctx, &telego.GetChatParams{ChatID: telego.ChatID{ID: chatID}})
	if err != nil || chat == nil || chat.ID != chatID || (chat.Type != "group" && chat.Type != "supergroup") {
		return errors.New("notification group is unavailable")
	}
	return nil
}

func (s *Service) SendNotification(ctx context.Context, chatID int64, text string) (int, error) {
	b, err := s.notificationBot()
	if err != nil {
		return 0, errors.New("notification bot is unavailable")
	}
	message, err := b.SendMessage(ctx, &telego.SendMessageParams{ChatID: telego.ChatID{ID: chatID}, Text: text, LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true}})
	if err != nil || message == nil || message.Chat.ID != chatID {
		return 0, errors.New("notification send failed")
	}
	return message.MessageID, nil
}
