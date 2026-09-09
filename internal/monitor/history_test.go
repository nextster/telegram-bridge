package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/nextster/telegram-bridge/internal/config"
)

type historyRPC struct {
	request  *tg.MessagesGetHistoryRequest
	messages []tg.MessageClass
}

func (r *historyRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	request, ok := input.(*tg.MessagesGetHistoryRequest)
	if !ok {
		return errors.New("unexpected RPC")
	}
	r.request = request
	output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: r.messages}
	return nil
}

func historyMessage(id int, date int, chat int64) *tg.Message {
	return &tg.Message{ID: id, Date: date, PeerID: &tg.PeerChat{ChatID: chat}, Message: "fixture"}
}

func TestHistoryDatesAndRawPagination(t *testing.T) {
	rpc := &historyRPC{messages: []tg.MessageClass{historyMessage(10, 200, 1), historyMessage(9, 199, 1), historyMessage(8, 100, 1), historyMessage(7, 99, 1)}}
	s := &Service{cfg: config.Config{TelegramAPIID: 1, TelegramAPIHash: "fake"}, api: tg.NewClient(rpc), authorized: true, userID: 1}
	opts := HistoryOptions{Chat: "chat:1", Limit: 4, MinDate: time.Unix(100, 0), MaxDate: time.Unix(200, 0)}
	page, err := s.GetHistoryPage(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[0].ID != 9 || page.Messages[1].ID != 8 || page.HasMore || page.NextOffsetID != 0 {
		t.Fatalf("wrong range: %+v", page)
	}
	if rpc.request.OffsetDate != 200 || rpc.request.Limit != 4 {
		t.Fatal("upper bound not sent to Telegram")
	}
	rpc.messages = []tg.MessageClass{&tg.MessageService{ID: 9, Date: 199}, &tg.MessageEmpty{ID: 8}}
	opts.Limit = 2
	opts.OffsetID = 10
	page, err = s.GetHistoryPage(context.Background(), opts)
	if err != nil || len(page.Messages) != 0 || !page.HasMore || page.NextOffsetID != 8 {
		t.Fatal("service-only page lost cursor")
	}
	if rpc.request.OffsetID != 10 {
		t.Fatal("pagination offset not sent")
	}
	rpc.messages = []tg.MessageClass{historyMessage(10, 199, 1), historyMessage(9, 199, 2)}
	page, err = s.GetHistoryPage(context.Background(), opts)
	if err != nil || len(page.Messages) != 0 {
		t.Fatal("history crossed peer or offset boundary")
	}
	rpc.messages = nil
	page, err = s.GetHistoryPage(context.Background(), opts)
	if err != nil || page.HasMore {
		t.Fatal("empty page not exhausted")
	}
	opts.MaxDate = time.Unix(200, 500000000)
	rpc.messages = []tg.MessageClass{historyMessage(9, 200, 1)}
	page, err = s.GetHistoryPage(context.Background(), opts)
	if err != nil || len(page.Messages) != 1 || rpc.request.OffsetDate != 201 {
		t.Fatal("fractional upper bound lost whole-second Telegram message")
	}
	opts.MinDate = opts.MaxDate
	if _, err := s.GetHistoryPage(context.Background(), opts); err == nil {
		t.Fatal("invalid range accepted")
	}
}

func TestHistoryRecognizesImageDocumentsButNotStickers(t *testing.T) {
	for _, mime := range []string{"image/jpeg", "image/png", "image/webp"} {
		doc := &tg.Document{MimeType: mime}
		m := &tg.MessageMediaDocument{}
		m.SetDocument(doc)
		if telegramDocumentKind(m) != "image" {
			t.Fatal("image document invisible to history processing")
		}
		doc.ID, doc.Size = 1, 5
		a, _ := attachmentFromMessage(1, TelegramMessage{ID: 1, MediaKind: "image"}, &tg.Message{Media: m})
		if !a.Supported || a.Kind != "image" {
			t.Fatal("history image kind rejected by attachment processing")
		}
		doc.Attributes = []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{}}
		if telegramDocumentKind(m) != "sticker" {
			t.Fatal("sticker treated as generic image")
		}
	}
	m := &tg.MessageMediaDocument{}
	m.SetDocument(&tg.Document{MimeType: "application/pdf"})
	if telegramDocumentKind(m) != "document" {
		t.Fatal("unsupported document reclassified")
	}
}
