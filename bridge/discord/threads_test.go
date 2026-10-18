package bdiscord

import (
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/matterbridge-org/matterbridge/bridge/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	startedThread    = "100000000000000010" // started from the message with the same ID in general
	standaloneThread = "100000000000000020" // started without a message
	unnamedThread    = "100000000000000025" // started without a message, its name in general deleted
	otherChannelID   = "100000000000000005" // "random", also bridged
	otherThread      = "100000000000000030" // in random
	unbridgedThread  = "100000000000000040" // in a channel the bridge doesn't know
)

func newThreadsTestBdiscord(t *testing.T, cfg string) (*Bdiscord, *fakeDiscordAPI) {
	b, api := newTestBdiscord(t, "QuoteDisable=true\nUseUserName=true\n"+cfg)
	b.SetBool("PreserveThreading", true)
	b.channels = append(b.channels, &discordgo.Channel{ID: otherChannelID, Name: "random", Type: discordgo.ChannelTypeGuildText})
	api.addChannel(otherChannelID, channelJSON(otherChannelID, discordgo.ChannelTypeGuildText, ""))
	api.addChannel(startedThread, channelJSON(startedThread, discordgo.ChannelTypeGuildPublicThread, testChannelID))
	api.addMessage(testChannelID, startedThread, `{"id": "`+startedThread+`"}`)
	api.addChannel(standaloneThread, channelJSON(standaloneThread, discordgo.ChannelTypeGuildPrivateThread, testChannelID))
	// Discord posts the name of a thread started without a message in its channel, with the thread's ID
	api.addMessage(testChannelID, standaloneThread, `{"id": "`+standaloneThread+`", "type": 18, "content": "thread name",
		"message_reference": {"channel_id": "`+standaloneThread+`", "guild_id": "`+testGuildID+`"}}`)
	api.addChannel(unnamedThread, channelJSON(unnamedThread, discordgo.ChannelTypeGuildPublicThread, testChannelID))
	api.addChannel(otherThread, channelJSON(otherThread, discordgo.ChannelTypeGuildPublicThread, otherChannelID))
	api.addChannel(unbridgedThread, channelJSON(unbridgedThread, discordgo.ChannelTypeGuildPublicThread, "100000000000000099"))
	return b, api
}

// receive has the bridge receive msg from Discord, and returns what it relayed to the gateway.
func receive(t *testing.T, b *Bdiscord, msg *discordgo.Message) (config.Message, bool) {
	msg.GuildID = testGuildID
	if msg.Author == nil {
		msg.Author = &discordgo.User{ID: "200000000000000001", Username: "skud"}
	}
	if msg.Content == "" {
		msg.Content = "message " + msg.ID
	}
	b.messageCreate(b.c, &discordgo.MessageCreate{Message: msg})
	return relayed(t, b)
}

// sendFromIRC has the bridge send msg to general, and returns the ID it was sent as.
func sendFromIRC(t *testing.T, b *Bdiscord, msg config.Message) string {
	msg.Channel = "general"
	if msg.Username == "" {
		msg.Username = "skud: "
	}
	id, err := b.Send(msg)
	require.NoError(t, err)
	return id
}

func TestThreadMessagesAreRelayedAsReplyChains(t *testing.T) {
	b, api := newThreadsTestBdiscord(t, "")

	// a thread started from a message is a chain of replies to that message
	for _, id := range []string{"300000000000000001", "300000000000000002"} {
		msg, ok := receive(t, b, &discordgo.Message{ID: id, ChannelID: startedThread})
		require.True(t, ok)
		assert.Equal(t, "general", msg.Channel)
		assert.Equal(t, id, msg.ID)
		assert.Equal(t, startedThread, msg.ParentID)
	}

	// a thread started without one is a chain of replies to its first message, the first one relayed
	b.messageCreate(b.c, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "300000000000000010", ChannelID: standaloneThread, GuildID: testGuildID, Author: &discordgo.User{Username: "skud"}, // a sticker, say
	}})
	_, ok := relayed(t, b)
	require.False(t, ok)
	first, ok := receive(t, b, &discordgo.Message{ID: "300000000000000011", ChannelID: standaloneThread})
	require.True(t, ok)
	assert.Equal(t, "general", first.Channel)
	assert.Empty(t, first.ParentID)
	second, _ := receive(t, b, &discordgo.Message{ID: "300000000000000012", ChannelID: standaloneThread})
	assert.Equal(t, first.ID, second.ParentID)

	// the same when the thread's name was deleted from general
	first, _ = receive(t, b, &discordgo.Message{ID: "300000000000000016", ChannelID: unnamedThread})
	assert.Empty(t, first.ParentID)
	second, _ = receive(t, b, &discordgo.Message{ID: "300000000000000017", ChannelID: unnamedThread})
	assert.Equal(t, first.ID, second.ParentID)

	// inline replies in a thread keep their parent
	reply, _ := receive(t, b, &discordgo.Message{
		ID: "300000000000000013", ChannelID: standaloneThread,
		MessageReference: &discordgo.MessageReference{ChannelID: standaloneThread, MessageID: second.ID},
	})
	assert.Equal(t, second.ID, reply.ParentID)

	// threads in channels the bridge doesn't know are like those channels
	msg, _ := receive(t, b, &discordgo.Message{ID: "300000000000000021", ChannelID: unbridgedThread})
	assert.Empty(t, msg.Channel)
	assert.Empty(t, msg.ParentID)

	// and messages outside threads are unchanged
	msg, _ = receive(t, b, &discordgo.Message{ID: "300000000000000031", ChannelID: testChannelID})
	assert.Equal(t, "general", msg.Channel)
	assert.Empty(t, msg.ParentID)

	// each thread is looked up once
	assert.Len(t, api.calls(http.MethodGet, "/channels/"+standaloneThread), 1)
	assert.Len(t, api.calls(http.MethodGet, "/channels/"+testChannelID+"/messages/"+standaloneThread), 1)
	assert.Len(t, api.calls(http.MethodGet, "/channels/"+startedThread), 1)
	assert.Len(t, api.calls(http.MethodGet, "/channels/"+testChannelID+"/messages/"+startedThread), 1)
}

func TestThreadsNeedPreserveThreading(t *testing.T) {
	b, api := newThreadsTestBdiscord(t, "")
	b.SetBool("PreserveThreading", false)

	msg, ok := receive(t, b, &discordgo.Message{ID: "300000000000000001", ChannelID: standaloneThread})
	require.True(t, ok)
	assert.Empty(t, msg.Channel, "thread messages are left to the gateway to drop, as before")

	b.threadCreate(b.c, &discordgo.ThreadCreate{Channel: &discordgo.Channel{ID: startedThread, GuildID: testGuildID, ParentID: testChannelID}})
	assert.Empty(t, api.calls(http.MethodPut, "/channels/"+startedThread+"/thread-members/@me"))
	assert.Empty(t, api.calls(http.MethodGet, "/channels/"+standaloneThread))
}

func TestThreadSystemMessagesAreNotRelayed(t *testing.T) {
	b, _ := newThreadsTestBdiscord(t, "")

	// the thread's name, posted in general when a thread is started without a message
	_, ok := receive(t, b, &discordgo.Message{
		ID: "300000000000000001", ChannelID: testChannelID, Type: discordgo.MessageTypeThreadCreated, Content: "thread name",
		MessageReference: &discordgo.MessageReference{ChannelID: standaloneThread},
	})
	assert.False(t, ok)

	// the copy of the message a thread started from, at the top of the thread
	_, ok = receive(t, b, &discordgo.Message{
		ID: "300000000000000002", ChannelID: startedThread, Type: discordgo.MessageTypeThreadStarterMessage, Content: "started from",
		MessageReference: &discordgo.MessageReference{ChannelID: testChannelID, MessageID: startedThread},
	})
	assert.False(t, ok)

	_, ok = receive(t, b, &discordgo.Message{ID: "300000000000000003", ChannelID: testChannelID, Type: discordgo.MessageTypeReply})
	assert.True(t, ok, "other message types are relayed as before")
}

func TestThreadDeletesAreRelayed(t *testing.T) {
	b, _ := newThreadsTestBdiscord(t, "")

	b.messageDelete(b.c, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "300000000000000001", ChannelID: standaloneThread, GuildID: testGuildID}})
	msg, ok := relayed(t, b)
	require.True(t, ok)
	assert.Equal(t, config.EventMsgDelete, msg.Event)
	assert.Equal(t, "general", msg.Channel)
}

func TestNewThreadsAreJoined(t *testing.T) {
	b, api := newThreadsTestBdiscord(t, "")
	thread := func(id, parentID string) *discordgo.ThreadCreate {
		return &discordgo.ThreadCreate{
			Channel:      &discordgo.Channel{ID: id, GuildID: testGuildID, ParentID: parentID, Type: discordgo.ChannelTypeGuildPrivateThread},
			NewlyCreated: true,
		}
	}

	b.threadCreate(b.c, thread(startedThread, testChannelID))
	assert.Len(t, api.calls(http.MethodPut, "/channels/"+startedThread+"/thread-members/@me"), 1)

	// Discord sends THREAD_CREATE again when the bot is added to a thread
	b.threadCreate(b.c, thread(startedThread, testChannelID))
	assert.Len(t, api.calls(http.MethodPut, "/channels/"+startedThread+"/thread-members/@me"), 1)

	b.threadCreate(b.c, thread(unbridgedThread, "100000000000000099"))
	assert.Empty(t, api.calls(http.MethodPut, "/channels/"+unbridgedThread+"/thread-members/@me"), "only threads in bridged channels are joined")

	// the thread is known from then on
	sendFromIRC(t, b, config.Message{Text: "irc reply", ParentID: startedThread})
	assert.Equal(t, "/channels/"+startedThread+"/messages", api.last(t).path)
	assert.Empty(t, api.calls(http.MethodGet, "/channels/"+startedThread))
}

func TestRepliesFromOtherBridgesGoIntoThreads(t *testing.T) {
	b, api := newThreadsTestBdiscord(t, "")
	inThread, _ := receive(t, b, &discordgo.Message{ID: "300000000000000011", ChannelID: standaloneThread})

	// a reply to a message relayed from a thread goes into the thread, as a plain message
	id := sendFromIRC(t, b, config.Message{Text: "irc reply", ParentID: inThread.ID})
	sent := api.last(t)
	assert.Equal(t, "/channels/"+standaloneThread+"/messages", sent.path)
	assert.Equal(t, "skud: irc reply", sent.message.Content)
	assert.Nil(t, sent.message.Reference)

	// and so does a reply to that reply
	sendFromIRC(t, b, config.Message{Text: "irc reply to the reply", ParentID: id})
	assert.Equal(t, "/channels/"+standaloneThread+"/messages", api.last(t).path)

	// a reply to the message a thread started from goes into the thread
	sendFromIRC(t, b, config.Message{Text: "irc reply to the starter", ParentID: startedThread})
	assert.Equal(t, "/channels/"+startedThread+"/messages", api.last(t).path)

	// a reply to a message without a thread stays an inline reply, and the message is only looked up once
	for range 2 {
		sendFromIRC(t, b, config.Message{Text: "inline", ParentID: existingMessage})
		sent = api.last(t)
		assert.Equal(t, "/channels/"+testChannelID+"/messages", sent.path)
		require.NotNil(t, sent.message.Reference)
		assert.Equal(t, existingMessage, sent.message.Reference.MessageID)
	}
	assert.Len(t, api.calls(http.MethodGet, "/channels/"+existingMessage), 1)

	// threads in other channels are left alone
	sendFromIRC(t, b, config.Message{Text: "elsewhere", ParentID: otherThread})
	assert.Equal(t, "/channels/"+testChannelID+"/messages", api.last(t).path)

	// edits and deletes of messages sent into a thread are made there
	sendFromIRC(t, b, config.Message{ID: id, Text: "edited"})
	assert.Len(t, api.calls(http.MethodPatch, "/channels/"+standaloneThread+"/messages/"+id), 1)
	sendFromIRC(t, b, config.Message{ID: id, Event: config.EventMsgDelete})
	assert.Len(t, api.calls(http.MethodDelete, "/channels/"+standaloneThread+"/messages/"+id), 1)
}

func TestRepliesFromOtherBridgesGoIntoThreadsThroughWebhooks(t *testing.T) {
	b, api := newThreadsTestBdiscord(t, "AutoWebhooks=true")
	inThread, _ := receive(t, b, &discordgo.Message{ID: "300000000000000011", ChannelID: standaloneThread})
	webhook := "/webhooks/" + testWebhookID + "/token"

	id := sendFromIRC(t, b, config.Message{Text: "irc reply", Username: "RustyCloud", ParentID: inThread.ID})
	sent := api.last(t)
	assert.Equal(t, webhook, sent.path)
	assert.Equal(t, standaloneThread, sent.query.Get("thread_id"))
	assert.Equal(t, "RustyCloud", sent.message.Username, "webhooks keep the sender's name in threads")
	assert.Equal(t, "irc reply", sent.message.Content)

	sendFromIRC(t, b, config.Message{ID: id, Text: "edited", Username: "RustyCloud"})
	edits := api.calls(http.MethodPatch, webhook+"/messages/"+id)
	require.Len(t, edits, 1)
	assert.Equal(t, standaloneThread, edits[0].query.Get("thread_id"))

	sendFromIRC(t, b, config.Message{ID: id, Event: config.EventMsgDelete})
	assert.Len(t, api.calls(http.MethodDelete, "/channels/"+standaloneThread+"/messages/"+id), 1)
}

func TestRepliesGoToTheChannelWhenTheThreadIsGone(t *testing.T) {
	b, api := newThreadsTestBdiscord(t, "")
	inThread, _ := receive(t, b, &discordgo.Message{ID: "300000000000000011", ChannelID: standaloneThread})
	api.removeChannel(standaloneThread)

	sendFromIRC(t, b, config.Message{Text: "irc reply", ParentID: inThread.ID})
	assert.Len(t, api.calls(http.MethodPost, "/channels/"+standaloneThread+"/messages"), 1)
	sent := api.last(t)
	assert.Equal(t, "/channels/"+testChannelID+"/messages", sent.path)
	assert.Equal(t, "skud: irc reply", sent.message.Content)
}
