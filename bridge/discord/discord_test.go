package bdiscord

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/matterbridge-org/matterbridge/bridge"
	"github.com/matterbridge-org/matterbridge/bridge/config"
	"github.com/matterbridge-org/matterbridge/bridge/discord/transmitter"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testGuildID     = "100000000000000000"
	testChannelID   = "100000000000000001"
	existingMessage = "100000000000000002"
	deletedMessage  = "100000000000000003"
	testWebhookID   = "100000000000000004"
)

type sentDiscordMessage struct {
	Content   string `json:"content"`
	Username  string `json:"username"`
	Reference *struct {
		MessageID       string `json:"message_id"`
		FailIfNotExists *bool  `json:"fail_if_not_exists"`
	} `json:"message_reference"`
}

type apiRequest struct {
	method  string
	path    string
	query   url.Values
	message sentDiscordMessage // the body of message sends and edits
}

// fakeDiscordAPI is the part of the Discord REST API the bridge uses. It records every request,
// serves the channels and messages it is given as JSON, and validates replies like Discord does.
type fakeDiscordAPI struct {
	mu       sync.Mutex
	requests []apiRequest
	channels map[string]string // by channel ID
	messages map[string]string // by "channel ID/message ID"
	ids      int
}

var (
	snowflake    = regexp.MustCompile(`^[0-9]+$`)
	channelPath  = regexp.MustCompile(`^/channels/([0-9]+)$`)
	messagesPath = regexp.MustCompile(`^/channels/([0-9]+)/messages(?:/([0-9]+))?$`)
	joinPath     = regexp.MustCompile(`^/channels/([0-9]+)/thread-members/@me$`)
	webhookPath  = regexp.MustCompile(`^/webhooks/([0-9]+)/[^/]+(?:/messages/([0-9]+))?$`)
)

func (f *fakeDiscordAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	req := apiRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query()}
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		_ = json.NewDecoder(r.Body).Decode(&req.message)
	}
	f.requests = append(f.requests, req)

	unknown := func(code int, what string) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"code": %d, "message": "Unknown %s"}`, code, what)
	}
	created := func(channelID string) {
		f.ids++
		fmt.Fprintf(w, `{"id": "2%017d", "channel_id": %q}`, f.ids, channelID)
	}

	switch {
	case r.Method == http.MethodGet && channelPath.MatchString(r.URL.Path):
		id := channelPath.FindStringSubmatch(r.URL.Path)[1]
		if channel, ok := f.channels[id]; ok {
			fmt.Fprint(w, channel)
			return
		}
		unknown(10003, "Channel")
	case messagesPath.MatchString(r.URL.Path):
		match := messagesPath.FindStringSubmatch(r.URL.Path)
		channelID, messageID := match[1], match[2]
		if _, ok := f.channels[channelID]; !ok {
			unknown(10003, "Channel")
			return
		}
		switch r.Method {
		case http.MethodGet:
			if msg, ok := f.messages[channelID+"/"+messageID]; ok {
				fmt.Fprint(w, msg)
				return
			}
			unknown(10008, "Message")
		case http.MethodPost:
			if ref := req.message.Reference; ref != nil {
				// Discord rejects IDs that aren't snowflakes, and replies to missing messages
				// unless fail_if_not_exists is false
				_, exists := f.messages[channelID+"/"+ref.MessageID]
				failIfNotExists := ref.FailIfNotExists == nil || *ref.FailIfNotExists
				if !snowflake.MatchString(ref.MessageID) || !exists && failIfNotExists {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"code": 50035, "message": "Invalid Form Body"}`)
					return
				}
			}
			created(channelID)
		case http.MethodPatch:
			fmt.Fprintf(w, `{"id": %q, "channel_id": %q}`, messageID, channelID)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == http.MethodPut && joinPath.MatchString(r.URL.Path):
		w.WriteHeader(http.StatusNoContent)
	case webhookPath.MatchString(r.URL.Path):
		messageID := webhookPath.FindStringSubmatch(r.URL.Path)[2]
		channelID := testChannelID
		if threadID := r.URL.Query().Get("thread_id"); threadID != "" {
			if _, ok := f.channels[threadID]; !ok {
				unknown(10003, "Channel")
				return
			}
			channelID = threadID
		}
		if r.Method == http.MethodPatch {
			fmt.Fprintf(w, `{"id": %q, "channel_id": %q}`, messageID, channelID)
			return
		}
		created(channelID)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDiscordAPI) addChannel(id, json string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels[id] = json
}

func (f *fakeDiscordAPI) removeChannel(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.channels, id)
}

func (f *fakeDiscordAPI) addMessage(channelID, id, json string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[channelID+"/"+id] = json
}

// calls returns the requests made so far with the given method and path.
func (f *fakeDiscordAPI) calls(method, path string) []apiRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apiRequest
	for _, req := range f.requests {
		if req.method == method && req.path == path {
			out = append(out, req)
		}
	}
	return out
}

// last returns the last message sent, by the bot or through a webhook.
func (f *fakeDiscordAPI) last(t *testing.T) apiRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		req := f.requests[i]
		if req.method == http.MethodPost && (messagesPath.MatchString(req.path) || webhookPath.MatchString(req.path)) {
			return req
		}
	}
	t.Fatal("no message was sent")
	return apiRequest{}
}

// newTestBdiscord returns a Discord bridge that talks to a fake API, with one channel called "general".
func newTestBdiscord(t *testing.T, cfg string) (*Bdiscord, *fakeDiscordAPI) {
	api := &fakeDiscordAPI{
		channels: map[string]string{testChannelID: channelJSON(testChannelID, discordgo.ChannelTypeGuildText, "")},
		messages: map[string]string{},
	}
	api.addMessage(testChannelID, existingMessage, `{"id": "`+existingMessage+`"}`)
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	channels, webhooks := discordgo.EndpointChannels, discordgo.EndpointWebhooks
	discordgo.EndpointChannels = server.URL + "/channels/"
	discordgo.EndpointWebhooks = server.URL + "/webhooks/"
	t.Cleanup(func() { discordgo.EndpointChannels, discordgo.EndpointWebhooks = channels, webhooks })

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	br := bridge.New(&config.Bridge{Account: "discord.test"})
	br.Config = config.NewConfigFromString(logger, []byte("[discord.test]\n"+cfg))
	br.Log = logrus.NewEntry(logger)

	b := New(&bridge.Config{Bridge: br, Remote: make(chan config.Message, 10)}).(*Bdiscord)
	session, err := discordgo.New("Bot test")
	require.NoError(t, err)
	b.c = session
	b.guildID = testGuildID
	b.nick = "bridgebot"
	b.channels = []*discordgo.Channel{{ID: testChannelID, Name: "general", Type: discordgo.ChannelTypeGuildText}}
	b.transmitter = transmitter.New(session, testGuildID, "matterbridge", b.useAutoWebhooks)
	b.transmitter.AddWebhook(testChannelID, &discordgo.Webhook{ID: testWebhookID, Token: "token", ChannelID: testChannelID})
	return b, api
}

func channelJSON(id string, channelType discordgo.ChannelType, parentID string) string {
	return fmt.Sprintf(`{"id": %q, "guild_id": %q, "type": %d, "parent_id": %q}`, id, testGuildID, channelType, parentID)
}

// relayed returns the message the bridge relayed to the gateway, if any.
func relayed(t *testing.T, b *Bdiscord) (config.Message, bool) {
	select {
	case msg := <-b.Remote:
		return msg, true
	case <-time.After(100 * time.Millisecond):
		return config.Message{}, false
	}
}

func TestSendReplies(t *testing.T) {
	b, api := newTestBdiscord(t, "")

	for _, tc := range []struct {
		name     string
		parentID string
		replyTo  string
	}{
		{"reply to a discord message", existingMessage, existingMessage},
		{"reply to a deleted message", deletedMessage, deletedMessage},
		{"parent not in the gateway cache", config.ParentIDNotFound, ""},
		{"parent that never reached discord", "irc msgid1", ""},
		{"parent sent to discord in several parts", existingMessage + ";" + deletedMessage, existingMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Send(config.Message{Text: "reply", Username: "skud: ", Channel: "general", ParentID: tc.parentID})
			require.NoError(t, err, "the message was dropped")

			sent := api.last(t).message
			assert.Equal(t, "skud: reply", sent.Content)
			if tc.replyTo == "" {
				assert.Nil(t, sent.Reference)
				return
			}
			require.NotNil(t, sent.Reference)
			assert.Equal(t, tc.replyTo, sent.Reference.MessageID)
			require.NotNil(t, sent.Reference.FailIfNotExists)
			assert.False(t, *sent.Reference.FailIfNotExists)
		})
	}
}

func TestQuotesUseProcessedText(t *testing.T) {
	b, api := newTestBdiscord(t, "")
	api.addMessage(testChannelID, existingMessage, `{
		"id": "`+existingMessage+`", "channel_id": "`+testChannelID+`",
		"content": "<@111> look at <:pog:222> in <#`+testChannelID+`>",
		"author": {"id": "333", "username": "RustyCloud"},
		"mentions": [{"id": "111", "username": "skud"}]
	}`)

	// messageCreate hands handleQuote the reply's text with mentions and emotes replaced
	reply := &discordgo.Message{
		ChannelID:        testChannelID,
		Content:          "<@111> agreed <:pog:222>",
		MessageReference: &discordgo.MessageReference{ChannelID: testChannelID, MessageID: existingMessage},
	}
	assert.Equal(t, "@skud agreed :pog: (re @RustyCloud: @skud look at :pog: in #general)",
		b.handleQuote(b.c, reply, "@skud agreed :pog:"))
}

func TestThreadedRepliesInWebhookChannels(t *testing.T) {
	b, api := newTestBdiscord(t, "AutoWebhooks=true")

	// webhooks can't reply, so the bot sends the reply with a username meant as a webhook display name
	_, err := b.Send(config.Message{Text: "irc reply", Username: "RustyCloud", Channel: "general", ParentID: existingMessage})
	require.NoError(t, err)
	sent := api.last(t).message
	assert.Equal(t, "**RustyCloud** irc reply", sent.Content)
	require.NotNil(t, sent.Reference)
	assert.Equal(t, existingMessage, sent.Reference.MessageID)
}
