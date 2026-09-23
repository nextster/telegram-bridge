package bot

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/nextster/telegram-bridge/internal/db"
)

const (
	deletionShowCallbackPrefix = "delshow:"
	// One press of "Показать" sends at most this many archived messages...
	deletionPageMessages = 30
	// ...in at most about this many bot messages.
	deletionPageSends = 10
	deletionTextLimit = 3900
	captionLimit      = 1024
)

// PrivateMedia is the short-lived cache of direct-chat attachments.
type PrivateMedia interface {
	Open(ctx context.Context, owner int64, messageID int) (db.PrivateMedia, *os.File, bool, error)
	MarkSent(ctx context.Context, owner int64, messageID int, fileID string) error
}

func (s *Service) SetPrivateMedia(media PrivateMedia) {
	s.privateMedia = media
}

// NotifyDeletedMessages sends an archive alert only to the account owner. A
// single message is shown right away, with its attachment if it is still
// cached; several messages become a summary with a "Показать" button.
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
	if len(deletion.Messages) == 1 {
		return s.sendDeletedMessage(ctx, owner, deletion.Dialog, deletion.Messages[0])
	}
	_, err = s.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:      telego.ChatID{ID: owner},
		Text:        formatDeletionSummary(deletion),
		ReplyMarkup: deletionShowMarkup(deletion.AlertID, 0, "Показать"),
	})
	if err != nil {
		return fmt.Errorf("send deletion alert to owner chat %d: %w", owner, err)
	}
	return nil
}

func (s *Service) sendDeletedMessage(ctx context.Context, owner int64, dialog db.PrivateDialog, message db.PrivateMessage) error {
	header := formatDeletedHeader(dialog, message)
	caption := header
	if text := strings.TrimSpace(message.Text); text != "" {
		caption += "\n\n" + text
	}
	if media, ok := s.openCachedMedia(ctx, owner, message); ok && s.sendCachedMedia(ctx, owner, message, media, caption) {
		return nil
	}
	_, err := s.bot.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: owner},
		Text:   truncateUTF16(header+"\n\n"+truncateUTF16(deletedMessageContent(message), 3500), deletionTextLimit),
	})
	if err != nil {
		return fmt.Errorf("send deletion alert to owner chat %d: %w", owner, err)
	}
	return nil
}

func (s *Service) handleDeletionShowCallback(ctx context.Context, query *telego.CallbackQuery, userID int64, raw string) error {
	alertPart, offsetPart, _ := strings.Cut(strings.TrimSpace(raw), ":")
	alertID, err := strconv.ParseInt(alertPart, 10, 64)
	if err != nil || alertID <= 0 {
		return s.answerCallback(ctx, query.ID, "Кнопка устарела.")
	}
	offset := 0
	if offsetPart != "" {
		if offset, err = strconv.Atoi(offsetPart); err != nil || offset < 0 {
			return s.answerCallback(ctx, query.ID, "Кнопка устарела.")
		}
	}
	// The alert is looked up only among the presser's own alerts.
	deletion, ok, err := s.store.GetPrivateDeletionAlert(ctx, userID, alertID)
	if err != nil {
		_ = s.answerCallback(ctx, query.ID, "Не получилось показать сообщения.")
		return err
	}
	if !ok || offset >= len(deletion.Messages) {
		s.clearCallbackMarkup(ctx, query)
		return s.answerCallback(ctx, query.ID, "Этих сообщений уже нет в архиве.")
	}
	if err := s.answerCallback(ctx, query.ID, ""); err != nil {
		log.Printf("answer deletion show callback failed: %v", err)
	}
	s.clearCallbackMarkup(ctx, query)
	return s.showDeletedMessages(ctx, userID, deletion, offset)
}

// showDeletedMessages sends one page of an alert: text grouped into few
// messages, attachments that are still cached in their place.
func (s *Service) showDeletedMessages(ctx context.Context, owner int64, deletion db.PrivateMessageDeletion, offset int) error {
	var buffer strings.Builder
	sends := 0
	flush := func() error {
		if buffer.Len() == 0 {
			return nil
		}
		err := s.reply(ctx, owner, buffer.String(), nil)
		buffer.Reset()
		sends++
		return err
	}

	next := offset
	for ; next < len(deletion.Messages) && next-offset < deletionPageMessages && sends < deletionPageSends; next++ {
		message := deletion.Messages[next]
		if media, ok := s.openCachedMedia(ctx, owner, message); ok {
			if err := flush(); err != nil {
				media.close()
				return err
			}
			caption := deletedAuthor(deletion.Dialog, message) + ":"
			if text := strings.TrimSpace(message.Text); text != "" {
				caption += " " + text
			}
			if s.sendCachedMedia(ctx, owner, message, media, caption) {
				sends++
				continue
			}
		}
		entry := truncateUTF16(deletedMessageLine(deletion.Dialog, message), 3500)
		if buffer.Len() > 0 && utf16Len(buffer.String())+utf16Len(entry)+2 > deletionTextLimit {
			if err := flush(); err != nil {
				return err
			}
		}
		if buffer.Len() > 0 {
			buffer.WriteString("\n\n")
		}
		buffer.WriteString(entry)
	}
	if err := flush(); err != nil {
		return err
	}
	if remaining := len(deletion.Messages) - next; remaining > 0 {
		text := fmt.Sprintf("Показано %d из %d.", next, len(deletion.Messages))
		return s.reply(ctx, owner, text, deletionShowMarkup(deletion.AlertID, next, fmt.Sprintf("Показать ещё %d", remaining)))
	}
	return nil
}

type cachedMedia struct {
	item db.PrivateMedia
	// file is nil when the bot resends the attachment by its file ID.
	file *os.File
}

func (m cachedMedia) close() {
	if m.file != nil {
		m.file.Close()
	}
}

// openCachedMedia finds the attachment of message in the owner's cache.
func (s *Service) openCachedMedia(ctx context.Context, owner int64, message db.PrivateMessage) (cachedMedia, bool) {
	if s.privateMedia == nil || !sendableMediaType(message.MediaType) {
		return cachedMedia{}, false
	}
	item, file, ok, err := s.privateMedia.Open(ctx, owner, message.MessageID)
	if err != nil {
		log.Printf("deleted message %d: attachment unavailable: %v", message.MessageID, err)
	}
	return cachedMedia{item: item, file: file}, ok && err == nil
}

// sendCachedMedia sends an opened attachment with caption to the owner and
// closes it. False means nothing reached the owner, so the caller falls back
// to text.
func (s *Service) sendCachedMedia(ctx context.Context, owner int64, message db.PrivateMessage, media cachedMedia, caption string) bool {
	defer media.close()
	item, file := media.item, media.file
	caption = strings.TrimSpace(caption)
	captionSent := false
	// Video notes and stickers take no caption; a long caption does not fit.
	if caption != "" && (!captionSupported(item.Kind) || utf16Len(caption) > captionLimit) {
		if err := s.reply(ctx, owner, truncateUTF16(caption, deletionTextLimit), nil); err != nil {
			log.Printf("deleted message %d: caption not sent: %v", message.MessageID, err)
			return false
		}
		caption, captionSent = "", true
	}
	sent, err := s.sendMedia(ctx, owner, item, file, caption, item.Kind)
	if err != nil && file != nil && item.Kind != "file" {
		// Telegram can refuse a file as voice, video note or sticker; as a
		// plain document it still reaches the owner.
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr == nil {
			log.Printf("deleted message %d: %s rejected, sending as document: %v", message.MessageID, item.Kind, err)
			if _, err = s.sendMedia(ctx, owner, item, file, caption, "file"); err == nil {
				return true
			}
		}
	}
	if err != nil {
		log.Printf("deleted message %d: attachment not sent: %v", message.MessageID, err)
		// If the caption already went out as text, do not repeat it.
		return captionSent
	}
	if file != nil {
		if fileID := sentFileID(sent); fileID != "" {
			if err := s.privateMedia.MarkSent(ctx, owner, message.MessageID, fileID); err != nil {
				log.Printf("deleted message %d: keep local attachment: %v", message.MessageID, err)
			}
		}
	}
	return true
}

func (s *Service) sendMedia(ctx context.Context, chatID int64, item db.PrivateMedia, file *os.File, caption, kind string) (*telego.Message, error) {
	chat := telego.ChatID{ID: chatID}
	input := tu.FileFromID(item.BotFileID)
	if file != nil {
		input = tu.File(tu.NameReader(file, mediaFileName(item)))
	}
	switch kind {
	case "photo":
		return s.bot.SendPhoto(ctx, tu.Photo(chat, input).WithCaption(caption))
	case "voice":
		return s.bot.SendVoice(ctx, tu.Voice(chat, input).WithCaption(caption).WithDuration(item.Duration))
	case "video_note":
		params := tu.VideoNote(chat, input).WithDuration(item.Duration)
		if item.Width > 0 {
			params = params.WithLength(item.Width)
		}
		return s.bot.SendVideoNote(ctx, params)
	case "video":
		return s.bot.SendVideo(ctx, tu.Video(chat, input).WithCaption(caption).WithDuration(item.Duration).
			WithWidth(item.Width).WithHeight(item.Height).WithSupportsStreaming())
	case "audio":
		return s.bot.SendAudio(ctx, tu.Audio(chat, input).WithCaption(caption).WithDuration(item.Duration))
	case "animation":
		return s.bot.SendAnimation(ctx, tu.Animation(chat, input).WithCaption(caption).WithDuration(item.Duration).
			WithWidth(item.Width).WithHeight(item.Height))
	case "sticker":
		return s.bot.SendSticker(ctx, tu.Sticker(chat, input))
	default:
		return s.bot.SendDocument(ctx, tu.Document(chat, input).WithCaption(caption))
	}
}

// sentFileID is the Bot API ID of the file in a message the bot just sent.
func sentFileID(message *telego.Message) string {
	switch {
	case message == nil:
		return ""
	case len(message.Photo) > 0:
		return message.Photo[len(message.Photo)-1].FileID
	case message.Voice != nil:
		return message.Voice.FileID
	case message.VideoNote != nil:
		return message.VideoNote.FileID
	case message.Video != nil:
		return message.Video.FileID
	case message.Audio != nil:
		return message.Audio.FileID
	case message.Animation != nil:
		return message.Animation.FileID
	case message.Sticker != nil:
		return message.Sticker.FileID
	case message.Document != nil:
		return message.Document.FileID
	default:
		return ""
	}
}

func deletionShowMarkup(alertID int64, offset int, label string) telego.ReplyMarkup {
	if alertID <= 0 {
		return nil
	}
	data := deletionShowCallbackPrefix + strconv.FormatInt(alertID, 10)
	if offset > 0 {
		data += ":" + strconv.Itoa(offset)
	}
	return tu.InlineKeyboard(tu.InlineKeyboardRow(tu.InlineKeyboardButton(label).WithCallbackData(data)))
}

// Telegram does not say who deleted a message, so the wording never names
// the actor.
func formatDeletionSummary(deletion db.PrivateMessageDeletion) string {
	count := len(deletion.Messages)
	own := 0
	for _, message := range deletion.Messages {
		if message.Outgoing {
			own++
		}
	}
	text := fmt.Sprintf("🫥 %s: удалено %d %s", privateDialogLabel(deletion.Dialog), count,
		russianPlural(count, "сообщение", "сообщения", "сообщений"))
	if own > 0 {
		text += fmt.Sprintf(" (ваших: %d)", own)
	}
	return truncateUTF16(text, deletionTextLimit)
}

func formatDeletedHeader(dialog db.PrivateDialog, message db.PrivateMessage) string {
	if message.Outgoing {
		return fmt.Sprintf("🫥 %s: удалено ваше сообщение", privateDialogLabel(dialog))
	}
	return fmt.Sprintf("🫥 %s: удалено сообщение", privateDialogLabel(dialog))
}

func deletedMessageLine(dialog db.PrivateDialog, message db.PrivateMessage) string {
	return deletedAuthor(dialog, message) + ": " + deletedMessageContent(message)
}

func deletedAuthor(dialog db.PrivateDialog, message db.PrivateMessage) string {
	if message.Outgoing {
		return "Вы"
	}
	if title := strings.TrimSpace(dialog.Title); title != "" {
		return title
	}
	if username := telegramUsername(dialog.Username); username != "" {
		return "@" + username
	}
	return "Собеседник"
}

// deletedMessageContent is the text of a message, marked with its media kind.
func deletedMessageContent(message db.PrivateMessage) string {
	text := strings.TrimSpace(message.Text)
	label := deletedMediaLabel(message.MediaType)
	switch {
	case label != "" && text != "":
		return label + " " + text
	case label != "":
		return label
	case text != "":
		return text
	default:
		return "[сообщение без текста]"
	}
}

func deletedMediaLabel(mediaType string) string {
	switch strings.TrimSpace(mediaType) {
	case "":
		return ""
	case "photo":
		return "[фото]"
	case "voice":
		return "[голосовое]"
	case "video_note":
		return "[видеокружок]"
	case "video":
		return "[видео]"
	case "audio":
		return "[аудио]"
	case "animation":
		return "[GIF]"
	case "sticker":
		return "[стикер]"
	case "file":
		return "[файл]"
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
	default:
		return "[медиа]"
	}
}

func sendableMediaType(mediaType string) bool {
	switch mediaType {
	case "photo", "voice", "video_note", "video", "audio", "animation", "sticker", "file":
		return true
	}
	return false
}

func captionSupported(kind string) bool {
	return kind != "video_note" && kind != "sticker"
}

func mediaFileName(item db.PrivateMedia) string {
	if name := strings.TrimSpace(item.FileName); name != "" && !strings.ContainsAny(name, "/\\") {
		return name
	}
	switch item.Kind {
	case "photo":
		return "photo.jpg"
	case "voice":
		return "voice.ogg"
	case "video_note":
		return "video_note.mp4"
	case "video", "animation":
		return item.Kind + ".mp4"
	case "audio":
		return "audio.mp3"
	case "sticker":
		switch item.MIME {
		case "application/x-tgsticker":
			return "sticker.tgs"
		case "video/webm":
			return "sticker.webm"
		}
		return "sticker.webp"
	default:
		return "file"
	}
}

func russianPlural(n int, one, few, many string) string {
	if n < 0 {
		n = -n
	}
	if n%100 >= 11 && n%100 <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	}
	return many
}

func utf16Len(value string) int {
	return len(utf16.Encode([]rune(value)))
}
