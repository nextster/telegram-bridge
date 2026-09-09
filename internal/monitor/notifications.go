package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gotd/td/tg"
)

// Notifications reuse the live authorized user client; never open another session.
func (s *Service) CheckNotificationChat(ctx context.Context, chatID int64) error {
	api, userID, err := s.readyAPI()
	if err != nil {
		return err
	}
	_, err = s.notificationPeer(ctx, api, userID, chatID)
	return err
}

func (s *Service) SendNotification(ctx context.Context, chatID int64, text, eventID string) (int, error) {
	api, userID, err := s.readyAPI()
	if err != nil {
		return 0, err
	}
	peer, err := s.notificationPeer(ctx, api, userID, chatID)
	if err != nil {
		return 0, err
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("notification:%d:%d:%s", userID, chatID, eventID)))
	randomID := int64(binary.LittleEndian.Uint64(digest[:8]))
	if randomID == 0 {
		randomID = 1
	}
	request := &tg.MessagesSendMessageRequest{Peer: peer, Message: text, RandomID: randomID, NoWebpage: true}
	// Override any saved channel/anonymous-admin send-as identity for supergroups.
	if _, ok := peer.(*tg.InputPeerChannel); ok {
		request.SetSendAs(&tg.InputPeerSelf{})
	}
	updates, err := api.MessagesSendMessage(ctx, request)
	if err != nil {
		return 0, errors.New("notification send through user session failed")
	}
	messageID := sentMessageID(updates)
	if messageID <= 0 {
		return 0, errors.New("notification response has no message ID")
	}
	return messageID, nil
}

func (s *Service) notificationPeer(ctx context.Context, api *tg.Client, userID, chatID int64) (tg.InputPeerClass, error) {
	var key string
	switch {
	case chatID > -1000000000000 && chatID < 0:
		key = fmt.Sprintf("chat:%d", -chatID)
	case chatID < -1000000000000 && chatID >= -1997852516352:
		key = fmt.Sprintf("channel:%d", -chatID-1000000000000)
	default:
		return nil, errors.New("invalid notification group")
	}
	peer, err := s.resolvePeer(ctx, userID, key)
	if err != nil {
		return nil, err
	}
	var result tg.MessagesChatsClass
	switch p := peer.(type) {
	case *tg.InputPeerChat:
		result, err = api.MessagesGetChats(ctx, []int64{p.ChatID})
	case *tg.InputPeerChannel:
		result, err = api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash}})
	}
	if err != nil {
		return nil, errors.New("notification group is unavailable to the authorized account")
	}
	var chats []tg.ChatClass
	switch r := result.(type) {
	case *tg.MessagesChats:
		chats = r.Chats
	case *tg.MessagesChatsSlice:
		chats = r.Chats
	}
	for _, item := range chats {
		switch chat := item.(type) {
		case *tg.Chat:
			if p, ok := peer.(*tg.InputPeerChat); ok && chat.ID == p.ChatID && !chat.Left && !chat.Deactivated {
				return peer, nil
			}
		case *tg.Channel:
			if p, ok := peer.(*tg.InputPeerChannel); ok && chat.ID == p.ChannelID && chat.Megagroup && !chat.Broadcast && !chat.Left {
				return peer, nil
			}
		}
	}
	return nil, errors.New("destination is not an accessible group")
}
