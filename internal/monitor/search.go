package monitor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
)

const maxQueryLimit = 100

// TelegramPeer is a stable, non-secret reference to a Telegram dialog.
// Key can be passed back to SearchMessages and GetHistory.
type TelegramPeer struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Username string `json:"username,omitempty"`
	Kind     string `json:"kind"`
}

type TelegramDialog struct {
	Peer        TelegramPeer `json:"peer"`
	TopMessage  int          `json:"top_message_id,omitempty"`
	UnreadCount int          `json:"unread_count,omitempty"`
	Pinned      bool         `json:"pinned,omitempty"`
}

type TelegramMessage struct {
	ID        int          `json:"id"`
	Chat      TelegramPeer `json:"chat"`
	Sender    string       `json:"sender,omitempty"`
	Date      time.Time    `json:"date"`
	EditedAt  *time.Time   `json:"edited_at,omitempty"`
	Text      string       `json:"text"`
	ReplyToID int          `json:"reply_to_id,omitempty"`
	TopicID   int          `json:"topic_id,omitempty"`
	MediaKind string       `json:"media_kind"`
	Outgoing  bool         `json:"outgoing,omitempty"`
}

type MessageSearchOptions struct {
	Chat     string
	Query    string
	Limit    int
	MinDate  time.Time
	MaxDate  time.Time
	OffsetID int
}

type HistoryOptions struct {
	Chat     string
	Limit    int
	OffsetID int
}

type peerDirectory map[string]TelegramPeer

func (s *Service) ListDialogs(ctx context.Context, query string, limit int) ([]TelegramDialog, error) {
	api, userID, err := s.readyAPI()
	if err != nil {
		return nil, err
	}
	limit = normalizeLimit(limit, 50)
	query = strings.TrimSpace(query)
	if query != "" {
		result, err := api.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: query, Limit: limit})
		if err != nil {
			return nil, fmt.Errorf("search Telegram dialogs: %w", err)
		}
		directory, err := s.rememberEntities(ctx, userID, result.Chats, result.Users)
		if err != nil {
			return nil, err
		}
		peers := append(append([]tg.PeerClass(nil), result.MyResults...), result.Results...)
		out := make([]TelegramDialog, 0, min(limit, len(peers)))
		seen := make(map[string]bool, len(peers))
		for _, item := range peers {
			peerType, peerID := peerInfo(item)
			key := peerKey(peerType, peerID)
			peer, ok := directory[key]
			if !ok || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, TelegramDialog{Peer: peer})
			if len(out) == limit {
				break
			}
		}
		return out, nil
	}

	result, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      maxQueryLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("get Telegram dialogs: %w", err)
	}
	dialogs, chats, users := dialogParts(result)
	directory, err := s.rememberEntities(ctx, userID, chats, users)
	if err != nil {
		return nil, err
	}
	out := make([]TelegramDialog, 0, limit)
	for _, item := range dialogs {
		dialog, ok := item.(*tg.Dialog)
		if !ok {
			continue
		}
		peerType, peerID := peerInfo(dialog.Peer)
		peer, ok := directory[peerKey(peerType, peerID)]
		if !ok {
			continue
		}
		out = append(out, TelegramDialog{
			Peer:        peer,
			TopMessage:  dialog.TopMessage,
			UnreadCount: dialog.UnreadCount,
			Pinned:      dialog.Pinned,
		})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *Service) SearchMessages(ctx context.Context, opts MessageSearchOptions) ([]TelegramMessage, error) {
	api, userID, err := s.readyAPI()
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return nil, errors.New("query must not be empty")
	}
	limit := normalizeLimit(opts.Limit, 20)
	minDate, maxDate := telegramDate(opts.MinDate), telegramDate(opts.MaxDate)

	var result tg.MessagesMessagesClass
	if strings.TrimSpace(opts.Chat) == "" {
		result, err = api.MessagesSearchGlobal(ctx, &tg.MessagesSearchGlobalRequest{
			Q:          query,
			Filter:     &tg.InputMessagesFilterEmpty{},
			MinDate:    minDate,
			MaxDate:    maxDate,
			OffsetPeer: &tg.InputPeerEmpty{},
			OffsetID:   opts.OffsetID,
			Limit:      limit,
		})
	} else {
		peer, resolveErr := s.resolvePeer(ctx, userID, opts.Chat)
		if resolveErr != nil {
			return nil, resolveErr
		}
		result, err = api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
			Peer:     peer,
			Q:        query,
			Filter:   &tg.InputMessagesFilterEmpty{},
			MinDate:  minDate,
			MaxDate:  maxDate,
			OffsetID: opts.OffsetID,
			Limit:    limit,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("search Telegram messages: %w", err)
	}
	return s.messagesFromResult(ctx, userID, result)
}

func (s *Service) GetHistory(ctx context.Context, opts HistoryOptions) ([]TelegramMessage, error) {
	api, userID, err := s.readyAPI()
	if err != nil {
		return nil, err
	}
	peer, err := s.resolvePeer(ctx, userID, opts.Chat)
	if err != nil {
		return nil, err
	}
	result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     peer,
		OffsetID: opts.OffsetID,
		Limit:    normalizeLimit(opts.Limit, 30),
	})
	if err != nil {
		return nil, fmt.Errorf("get Telegram history: %w", err)
	}
	return s.messagesFromResult(ctx, userID, result)
}

func (s *Service) messagesFromResult(ctx context.Context, userID int64, result tg.MessagesMessagesClass) ([]TelegramMessage, error) {
	messages, chats, users := messageParts(result)
	directory, err := s.rememberEntities(ctx, userID, chats, users)
	if err != nil {
		return nil, err
	}
	out := make([]TelegramMessage, 0, len(messages))
	for _, item := range messages {
		message, ok := item.(*tg.Message)
		if !ok {
			continue
		}
		peerType, peerID := peerInfo(message.PeerID)
		chat := directory[peerKey(peerType, peerID)]
		if chat.Key == "" {
			chat = TelegramPeer{Key: peerKey(peerType, peerID), Type: peerType, ID: peerID, Title: peerKey(peerType, peerID), Kind: peerType}
		}
		sender := ""
		if message.FromID != nil {
			senderType, senderID := peerInfo(message.FromID)
			if item, ok := directory[peerKey(senderType, senderID)]; ok {
				sender = item.Title
			} else {
				sender = peerKey(senderType, senderID)
			}
		}
		replyToID, topicID := telegramReplyMetadata(message)
		out = append(out, TelegramMessage{
			ID:        message.ID,
			Chat:      chat,
			Sender:    sender,
			Date:      unixTime(message.Date),
			EditedAt:  telegramEditedAt(message),
			Text:      message.Message,
			ReplyToID: replyToID,
			TopicID:   topicID,
			MediaKind: telegramMediaKind(message.Media),
			Outgoing:  message.Out,
		})
	}
	return out, nil
}

func telegramReplyMetadata(message *tg.Message) (replyToID, topicID int) {
	reply, ok := message.GetReplyTo()
	if !ok {
		return 0, 0
	}
	header, ok := reply.(*tg.MessageReplyHeader)
	if !ok {
		return 0, 0
	}
	if value, ok := header.GetReplyToMsgID(); ok && value > 0 {
		replyToID = value
	}
	if !header.GetForumTopic() {
		return replyToID, 0
	}
	if value, ok := header.GetReplyToTopID(); ok && value > 0 {
		return replyToID, value
	}
	// Telegram may omit reply_to_top_id for a direct reply to a forum
	// topic's root message because reply_to_msg_id is already the topic ID.
	return replyToID, replyToID
}

func telegramEditedAt(message *tg.Message) *time.Time {
	seconds, ok := message.GetEditDate()
	if !ok || seconds <= 0 {
		return nil
	}
	editedAt := time.Unix(int64(seconds), 0).UTC()
	return &editedAt
}

func telegramMediaKind(media tg.MessageMediaClass) string {
	switch media := media.(type) {
	case nil, *tg.MessageMediaEmpty:
		return "text"
	case *tg.MessageMediaPhoto:
		return "photo"
	case *tg.MessageMediaDocument:
		return telegramDocumentKind(media)
	case *tg.MessageMediaWebPage:
		return "web_page"
	case *tg.MessageMediaGeo:
		return "location"
	case *tg.MessageMediaGeoLive:
		return "live_location"
	case *tg.MessageMediaVenue:
		return "venue"
	case *tg.MessageMediaContact:
		return "contact"
	case *tg.MessageMediaPoll:
		return "poll"
	case *tg.MessageMediaDice:
		return "dice"
	case *tg.MessageMediaGame:
		return "game"
	case *tg.MessageMediaInvoice:
		return "invoice"
	case *tg.MessageMediaStory:
		return "story"
	case *tg.MessageMediaGiveaway:
		return "giveaway"
	case *tg.MessageMediaGiveawayResults:
		return "giveaway_results"
	case *tg.MessageMediaPaidMedia:
		return "paid_media"
	case *tg.MessageMediaToDo:
		return "todo"
	case *tg.MessageMediaVideoStream:
		return "video_stream"
	case *tg.MessageMediaUnsupported:
		return "unsupported"
	default:
		return "media"
	}
}

func telegramDocumentKind(media *tg.MessageMediaDocument) string {
	customEmoji := false
	sticker := false
	animated := false
	round := media.GetRound()
	voice := media.GetVoice()
	video := media.GetVideo()
	audio := false

	if documentClass, ok := media.GetDocument(); ok {
		if document, ok := documentClass.(*tg.Document); ok {
			for _, attribute := range document.Attributes {
				switch attribute := attribute.(type) {
				case *tg.DocumentAttributeCustomEmoji:
					customEmoji = true
				case *tg.DocumentAttributeSticker:
					sticker = true
				case *tg.DocumentAttributeAnimated:
					animated = true
				case *tg.DocumentAttributeVideo:
					if attribute.GetRoundMessage() {
						round = true
					} else {
						video = true
					}
				case *tg.DocumentAttributeAudio:
					if attribute.GetVoice() {
						voice = true
					} else {
						audio = true
					}
				}
			}
		}
	}

	switch {
	case customEmoji:
		return "custom_emoji"
	case sticker:
		return "sticker"
	case round:
		return "video_note"
	case voice:
		return "voice"
	case animated:
		return "animation"
	case video:
		return "video"
	case audio:
		return "audio"
	default:
		return "document"
	}
}

func (s *Service) rememberEntities(ctx context.Context, userID int64, chats []tg.ChatClass, users []tg.UserClass) (peerDirectory, error) {
	directory := make(peerDirectory, len(chats)+len(users))
	for _, item := range chats {
		switch chat := item.(type) {
		case *tg.Channel:
			accessHash, _ := chat.GetAccessHash()
			username, _ := chat.GetUsername()
			kind := "channel"
			if chat.Megagroup {
				kind = "supergroup"
			}
			peer := TelegramPeer{Key: peerKey("channel", chat.ID), Type: "channel", ID: chat.ID, Title: chat.Title, Username: username, Kind: kind}
			directory[peer.Key] = peer
			if accessHash != 0 {
				if err := s.store.SetChannelAccessHash(ctx, userID, chat.ID, accessHash); err != nil {
					return nil, fmt.Errorf("save channel access hash: %w", err)
				}
			}
		case *tg.Chat:
			peer := TelegramPeer{Key: peerKey("chat", chat.ID), Type: "chat", ID: chat.ID, Title: chat.Title, Kind: "group"}
			directory[peer.Key] = peer
		}
	}
	for _, item := range users {
		user, ok := item.(*tg.User)
		if !ok {
			continue
		}
		username, _ := user.GetUsername()
		accessHash, _ := user.GetAccessHash()
		title := strings.TrimSpace(strings.Join([]string{user.FirstName, user.LastName}, " "))
		if title == "" {
			title = username
		}
		if title == "" {
			title = peerKey("user", user.ID)
		}
		peer := TelegramPeer{Key: peerKey("user", user.ID), Type: "user", ID: user.ID, Title: title, Username: username, Kind: "private"}
		directory[peer.Key] = peer
		if accessHash != 0 {
			if err := s.store.SetUserAccessHash(ctx, userID, user.ID, accessHash); err != nil {
				return nil, fmt.Errorf("save user access hash: %w", err)
			}
		}
	}
	return directory, nil
}

func (s *Service) resolvePeer(ctx context.Context, userID int64, key string) (tg.InputPeerClass, error) {
	peerType, peerID, err := parsePeerKey(key)
	if err != nil {
		return nil, err
	}
	switch peerType {
	case "chat":
		return &tg.InputPeerChat{ChatID: peerID}, nil
	case "channel":
		accessHash, ok, err := s.store.GetChannelAccessHash(ctx, userID, peerID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("unknown chat %q; call telegram_list_dialogs or telegram_search_messages first", key)
		}
		return &tg.InputPeerChannel{ChannelID: peerID, AccessHash: accessHash}, nil
	case "user":
		if peerID == userID {
			return &tg.InputPeerSelf{}, nil
		}
		accessHash, ok, err := s.store.GetUserAccessHash(ctx, userID, peerID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("unknown chat %q; call telegram_list_dialogs or telegram_search_messages first", key)
		}
		return &tg.InputPeerUser{UserID: peerID, AccessHash: accessHash}, nil
	default:
		return nil, fmt.Errorf("unsupported peer type %q", peerType)
	}
}

func parsePeerKey(key string) (string, int64, error) {
	peerType, rawID, ok := strings.Cut(strings.TrimSpace(key), ":")
	if !ok {
		return "", 0, errors.New("chat must use the key returned by Telegram tools, for example channel:123")
	}
	peerID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || peerID <= 0 {
		return "", 0, fmt.Errorf("invalid chat key %q", key)
	}
	return peerType, peerID, nil
}

func peerKey(peerType string, peerID int64) string { return fmt.Sprintf("%s:%d", peerType, peerID) }

func normalizeLimit(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	if value > maxQueryLimit {
		return maxQueryLimit
	}
	return value
}

func telegramDate(value time.Time) int {
	if value.IsZero() {
		return 0
	}
	return int(value.Unix())
}

func dialogParts(result tg.MessagesDialogsClass) ([]tg.DialogClass, []tg.ChatClass, []tg.UserClass) {
	switch value := result.(type) {
	case *tg.MessagesDialogs:
		return value.Dialogs, value.Chats, value.Users
	case *tg.MessagesDialogsSlice:
		return value.Dialogs, value.Chats, value.Users
	default:
		return nil, nil, nil
	}
}

func messageParts(result tg.MessagesMessagesClass) ([]tg.MessageClass, []tg.ChatClass, []tg.UserClass) {
	switch value := result.(type) {
	case *tg.MessagesMessages:
		return value.Messages, value.Chats, value.Users
	case *tg.MessagesMessagesSlice:
		return value.Messages, value.Chats, value.Users
	case *tg.MessagesChannelMessages:
		return value.Messages, value.Chats, value.Users
	default:
		return nil, nil, nil
	}
}
