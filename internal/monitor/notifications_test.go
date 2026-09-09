package monitor

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
)

type notificationRPC struct {
	chats    []tg.ChatClass
	requests []*tg.MessagesSendMessageRequest
	fail     bool
}

func (r *notificationRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.MessagesGetChatsRequest, *tg.ChannelsGetChannelsRequest:
		output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{Chats: r.chats}
	case *tg.MessagesSendMessageRequest:
		r.requests = append(r.requests, request)
		if r.fail {
			return errors.New("sensitive transport details")
		}
		output.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: 42}
	default:
		return errors.New("unexpected RPC")
	}
	return nil
}
func notificationMonitor(t *testing.T) (*Service, *notificationRPC) {
	t.Helper()
	store, err := db.Open(context.Background(), t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.SetChannelAccessHash(context.Background(), 123, 1234567890, 987); err != nil {
		t.Fatal(err)
	}
	rpc := &notificationRPC{chats: []tg.ChatClass{&tg.Channel{ID: 1234567890, Megagroup: true}}}
	return &Service{cfg: config.Config{TelegramAPIID: 1, TelegramAPIHash: "fixture"}, store: store, api: tg.NewClient(rpc), userID: 123, authorized: true}, rpc
}
func TestNotificationUsesAuthorizedUserAndStableRandomID(t *testing.T) {
	s, rpc := notificationMonitor(t)
	for range 2 {
		id, err := s.SendNotification(context.Background(), -1001234567890, "Release text", "release:1")
		if err != nil || id != 42 {
			t.Fatalf("id=%d err=%v", id, err)
		}
	}
	for _, request := range rpc.requests {
		peer, ok := request.Peer.(*tg.InputPeerChannel)
		if !ok || peer.ChannelID != 1234567890 || peer.AccessHash != 987 {
			t.Fatal("wrong account-scoped peer")
		}
		if _, ok := request.SendAs.(*tg.InputPeerSelf); !ok {
			t.Fatal("must send as the authorized account")
		}
		if request.Message != "Release text" || !request.NoWebpage || request.RandomID == 0 {
			t.Fatal("incorrect send request")
		}
	}
	if rpc.requests[0].RandomID != rpc.requests[1].RandomID {
		t.Fatal("retry changed Telegram random_id")
	}
	if _, err := s.SendNotification(context.Background(), -1001234567890, "Next release", "release:2"); err != nil {
		t.Fatal(err)
	}
	if rpc.requests[0].RandomID == rpc.requests[2].RandomID {
		t.Fatal("different event reused Telegram random_id")
	}
}
func TestNotificationBasicGroupNeedsNoBot(t *testing.T) {
	s, rpc := notificationMonitor(t)
	rpc.chats = []tg.ChatClass{&tg.Chat{ID: 77}}
	if _, err := s.SendNotification(context.Background(), -77, "Text", "release:1"); err != nil {
		t.Fatal(err)
	}
	if peer, ok := rpc.requests[0].Peer.(*tg.InputPeerChat); !ok || peer.ChatID != 77 {
		t.Fatal("wrong basic group")
	}
}
func TestNotificationRejectsUnavailableUserAndWrongPeers(t *testing.T) {
	for _, chats := range [][]tg.ChatClass{
		{&tg.Channel{ID: 1234567890, Broadcast: true}},
		{&tg.Channel{ID: 1234567890, Megagroup: true, Left: true}},
		{&tg.Channel{ID: 111, Megagroup: true}},
		{&tg.ChannelForbidden{ID: 1234567890}},
		{},
	} {
		s, rpc := notificationMonitor(t)
		rpc.chats = chats
		if _, err := s.SendNotification(context.Background(), -1001234567890, "Text", "event"); err == nil {
			t.Fatal("accepted inaccessible group")
		}
		if len(rpc.requests) != 0 {
			t.Fatal("sent to invalid group")
		}
	}
	s, rpc := notificationMonitor(t)
	s.authorized = false
	if _, err := s.SendNotification(context.Background(), -1001234567890, "Text", "event"); err == nil {
		t.Fatal("accepted logged-out user")
	}
	if len(rpc.requests) != 0 {
		t.Fatal("sent without authorization")
	}
}
