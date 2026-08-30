package monitor

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gotd/td/telegram/message"
	telegrammarkdown "github.com/gotd/td/telegram/message/markdown"
	"github.com/gotd/td/tg"

	"github.com/nextster/telegram-bridge/internal/db"
)

const codexFolderTitle = "Codex"

const codexReadReconcileInterval = 15 * time.Second

func (s *Service) runCodexReadReconcile(ctx context.Context) {
	for {
		topics, receipts, err := s.ReconcileCodexReadState(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("Codex Telegram read-state reconcile failed: %v", err)
		} else if receipts > 0 {
			log.Printf("Codex Telegram read-state reconciled: topics=%d receipts=%d", topics, receipts)
		}
		timer := time.NewTimer(codexReadReconcileInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) ReconcileCodexReadState(ctx context.Context) (int, int, error) {
	api, _, err := s.readyAPI()
	if err != nil {
		return 0, 0, err
	}
	projects, err := s.store.ListCodexProjects(ctx)
	if err != nil {
		return 0, 0, err
	}
	var topics, receipts int
	for _, project := range projects {
		threads, err := s.store.ListCodexThreadsByChatID(ctx, project.TelegramChatID)
		if err != nil {
			return topics, receipts, err
		}
		peer := &tg.InputPeerChannel{ChannelID: project.TelegramChannelID, AccessHash: project.TelegramAccessHash}
		for start := 0; start < len(threads); start += 100 {
			end := min(start+100, len(threads))
			ids := make([]int, 0, end-start)
			for _, thread := range threads[start:end] {
				ids = append(ids, thread.TelegramTopicID)
			}
			result, err := api.MessagesGetForumTopicsByID(ctx, &tg.MessagesGetForumTopicsByIDRequest{Peer: peer, Topics: ids})
			if err != nil {
				return topics, receipts, fmt.Errorf("get Codex forum topics for %s: %w", project.Slug, err)
			}
			for _, item := range result.Topics {
				topic, ok := item.(*tg.ForumTopic)
				if !ok {
					continue
				}
				topics++
				deliver, err := s.store.ObserveCodexTopicReadState(ctx, project.TelegramChatID, topic.ID, topic.UnreadCount > 0, topic.ReadInboxMaxID)
				if err != nil {
					return topics, receipts, err
				}
				if deliver {
					receipts++
				}
			}
		}
	}
	return topics, receipts, nil
}

// SendCodexMessage mirrors a Codex user item through the authenticated user
// session, so Telegram displays the actual account as its sender.
func (s *Service) SendCodexMessage(ctx context.Context, project db.CodexProject, topicID int, markdown string) (int, error) {
	markdown = strings.TrimSpace(markdown)
	if project.TelegramChannelID <= 0 || project.TelegramAccessHash == 0 || topicID <= 0 || markdown == "" {
		return 0, errors.New("invalid Codex user message destination")
	}
	api, _, err := s.readyAPI()
	if err != nil {
		return 0, err
	}
	if err := s.store.ReserveCodexOutboundMessage(ctx, project.TelegramChatID, topicID, markdown); err != nil {
		return 0, err
	}
	peer := &tg.InputPeerChannel{ChannelID: project.TelegramChannelID, AccessHash: project.TelegramAccessHash}
	updates, err := message.NewSender(api).To(peer).Reply(topicID).RandomID(codexRandomID()).StyledText(ctx, telegrammarkdown.String(nil, markdown))
	if err != nil {
		s.store.CancelCodexOutboundMessage(ctx, project.TelegramChatID, topicID)
		return 0, fmt.Errorf("send Codex message as Telegram user: %w", err)
	}
	messageID := sentMessageID(updates)
	if messageID <= 0 {
		s.store.CancelCodexOutboundMessage(ctx, project.TelegramChatID, topicID)
		return 0, fmt.Errorf("send Codex message: response %T has no message id", updates)
	}
	if err := s.store.CompleteCodexOutboundMessage(ctx, project.TelegramChatID, topicID, messageID, markdown); err != nil {
		return 0, err
	}
	return messageID, nil
}

func (s *Service) MarkCodexTopicRead(ctx context.Context, project db.CodexProject, topicID, readMaxID int) error {
	if readMaxID <= 0 {
		return nil
	}
	api, _, err := s.readyAPI()
	if err != nil {
		return err
	}
	_, err = api.MessagesReadDiscussion(ctx, &tg.MessagesReadDiscussionRequest{
		Peer:  &tg.InputPeerChannel{ChannelID: project.TelegramChannelID, AccessHash: project.TelegramAccessHash},
		MsgID: topicID, ReadMaxID: readMaxID,
	})
	if err != nil {
		return fmt.Errorf("mark Codex Telegram topic read: %w", err)
	}
	return nil
}

func codexRandomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return int64(binary.LittleEndian.Uint64(b[:]))
	}
	return 0
}

func sentMessageID(updates tg.UpdatesClass) int {
	switch typed := updates.(type) {
	case *tg.UpdateShortSentMessage:
		return typed.ID
	case *tg.Updates:
		return sentMessageIDFromUpdates(typed.Updates)
	case *tg.UpdatesCombined:
		return sentMessageIDFromUpdates(typed.Updates)
	}
	return 0
}

func sentMessageIDFromUpdates(updates []tg.UpdateClass) int {
	for _, update := range updates {
		switch typed := update.(type) {
		case *tg.UpdateNewMessage:
			return typed.Message.GetID()
		case *tg.UpdateNewChannelMessage:
			return typed.Message.GetID()
		}
	}
	return 0
}

// EnsureCodexProject creates the Telegram-side project container through the
// already-authorized user session. It never starts a second gotd client.
func (s *Service) EnsureCodexProject(ctx context.Context, slug, title, botUsername string) (db.CodexProject, error) {
	slug = strings.TrimSpace(slug)
	title = strings.TrimSpace(title)
	botUsername = strings.TrimPrefix(strings.TrimSpace(botUsername), "@")
	if slug == "" {
		return db.CodexProject{}, errors.New("project slug is empty")
	}
	if existing, ok, err := s.store.GetCodexProject(ctx, slug); err != nil || ok {
		return existing, err
	}
	if title == "" {
		title = slug
	}
	api, _, err := s.readyAPI()
	if err != nil {
		return db.CodexProject{}, err
	}

	created, err := api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
		Megagroup: true,
		Forum:     true,
		Title:     truncateTelegramTitle("Codex · " + title),
		About:     "Codex tasks for " + title + ". Managed by telegram-bridge.",
	})
	if err != nil {
		return db.CodexProject{}, fmt.Errorf("create Codex project forum: %w", err)
	}
	channel, err := channelFromUpdates(created)
	if err != nil {
		return db.CodexProject{}, err
	}
	accessHash, ok := channel.GetAccessHash()
	if !ok || accessHash == 0 {
		return db.CodexProject{}, errors.New("created Codex forum has no access hash")
	}

	if botUsername != "" {
		if err := addForumBot(ctx, api, channel.AsInput(), botUsername); err != nil {
			return db.CodexProject{}, err
		}
	}

	project := db.CodexProject{
		Slug:               slug,
		Title:              title,
		TelegramChannelID:  channel.ID,
		TelegramAccessHash: accessHash,
		TelegramChatID:     botAPIChannelID(channel.ID),
	}
	if err := s.store.UpsertCodexProject(ctx, project); err != nil {
		return db.CodexProject{}, err
	}
	if err := s.ensureCodexFolder(ctx, api); err != nil {
		return db.CodexProject{}, err
	}
	saved, ok, err := s.store.GetCodexProject(ctx, slug)
	if err != nil {
		return db.CodexProject{}, err
	}
	if !ok {
		return db.CodexProject{}, errors.New("saved Codex project disappeared")
	}
	return saved, nil
}

func (s *Service) ensureCodexFolder(ctx context.Context, api *tg.Client) error {
	projects, err := s.store.ListCodexProjects(ctx)
	if err != nil {
		return err
	}
	include := make([]tg.InputPeerClass, 0, len(projects))
	for _, project := range projects {
		include = append(include, &tg.InputPeerChannel{ChannelID: project.TelegramChannelID, AccessHash: project.TelegramAccessHash})
	}
	filters, err := api.MessagesGetDialogFilters(ctx)
	if err != nil {
		return fmt.Errorf("get Telegram folders: %w", err)
	}
	filterID := 2
	used := map[int]bool{}
	for _, item := range filters.Filters {
		filter, ok := item.(*tg.DialogFilter)
		if !ok {
			continue
		}
		used[filter.ID] = true
		if strings.EqualFold(strings.TrimSpace(filter.Title.Text), codexFolderTitle) {
			filterID = filter.ID
			break
		}
	}
	if used[filterID] {
		for id := 2; id <= 255; id++ {
			if !used[id] {
				filterID = id
				break
			}
		}
		for _, item := range filters.Filters {
			if filter, ok := item.(*tg.DialogFilter); ok && strings.EqualFold(strings.TrimSpace(filter.Title.Text), codexFolderTitle) {
				filterID = filter.ID
				break
			}
		}
	}
	filter := &tg.DialogFilter{
		ID:           filterID,
		Title:        tg.TextWithEntities{Text: codexFolderTitle},
		IncludePeers: include,
	}
	filter.SetEmoticon("💻")
	ok, err := api.MessagesUpdateDialogFilter(ctx, &tg.MessagesUpdateDialogFilterRequest{ID: filterID, Filter: filter})
	if err != nil {
		return fmt.Errorf("update Codex Telegram folder: %w", err)
	}
	if !ok {
		return errors.New("Telegram declined Codex folder update")
	}
	return nil
}

func addForumBot(ctx context.Context, api *tg.Client, channel *tg.InputChannel, username string) error {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return fmt.Errorf("resolve bridge bot @%s: %w", username, err)
	}
	var bot *tg.User
	for _, item := range resolved.Users {
		if user, ok := item.(*tg.User); ok && user.Bot {
			bot = user
			break
		}
	}
	if bot == nil {
		return fmt.Errorf("@%s did not resolve to a bot", username)
	}
	inputBot := bot.AsInput()
	rights := tg.ChatAdminRights{ChangeInfo: true, DeleteMessages: true, InviteUsers: true, PinMessages: true, ManageTopics: true, Other: true}
	_, err = api.ChannelsEditAdmin(ctx, &tg.ChannelsEditAdminRequest{
		Channel:     channel,
		UserID:      inputBot,
		AdminRights: rights,
		Rank:        "Codex bridge",
	})
	if err != nil {
		return fmt.Errorf("add bridge bot as forum admin: %w", err)
	}
	return nil
}

func channelFromUpdates(updates tg.UpdatesClass) (*tg.Channel, error) {
	var chats []tg.ChatClass
	switch typed := updates.(type) {
	case *tg.Updates:
		chats = typed.Chats
	case *tg.UpdatesCombined:
		chats = typed.Chats
	default:
		return nil, fmt.Errorf("unexpected create forum response %T", updates)
	}
	for _, item := range chats {
		if channel, ok := item.(*tg.Channel); ok && channel.Megagroup {
			return channel, nil
		}
	}
	return nil, errors.New("create forum response did not contain a channel")
}

func botAPIChannelID(channelID int64) int64 {
	return -(1_000_000_000_000 + channelID)
}

func truncateTelegramTitle(title string) string {
	runes := []rune(strings.TrimSpace(title))
	if len(runes) > 128 {
		runes = runes[:128]
	}
	return string(runes)
}
