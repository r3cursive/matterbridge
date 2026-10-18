package bdiscord

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/matterbridge-org/matterbridge/bridge"
	"github.com/matterbridge-org/matterbridge/bridge/config"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testChannelID   = "100000000000000001"
	existingMessage = "100000000000000002"
	deletedMessage  = "100000000000000003"
)

type sentDiscordMessage struct {
	Content   string `json:"content"`
	Reference *struct {
		MessageID       string `json:"message_id"`
		FailIfNotExists *bool  `json:"fail_if_not_exists"`
	} `json:"message_reference"`
}

// fakeDiscordAPI records the messages the bot sends, and validates replies like Discord does.
type fakeDiscordAPI struct {
	mu   sync.Mutex
	sent []sentDiscordMessage
}

var snowflake = regexp.MustCompile(`^[0-9]+$`)

func (f *fakeDiscordAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/channels/"+testChannelID+"/messages" {
		http.NotFound(w, r)
		return
	}
	var msg sentDiscordMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ref := msg.Reference; ref != nil {
		// Discord rejects IDs that aren't snowflakes, and replies to missing messages
		// unless fail_if_not_exists is false
		failIfNotExists := ref.FailIfNotExists == nil || *ref.FailIfNotExists
		if !snowflake.MatchString(ref.MessageID) || ref.MessageID != existingMessage && failIfNotExists {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"code": 50035, "message": "Invalid Form Body"}`)
			return
		}
	}

	f.mu.Lock()
	f.sent = append(f.sent, msg)
	id := len(f.sent)
	f.mu.Unlock()
	fmt.Fprintf(w, `{"id": "2%017d", "channel_id": %q}`, id, testChannelID)
}

func (f *fakeDiscordAPI) last(t *testing.T) sentDiscordMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.sent)
	return f.sent[len(f.sent)-1]
}

// newTestBdiscord returns a Discord bridge that talks to a fake API, with one channel called "general".
func newTestBdiscord(t *testing.T, cfg string) (*Bdiscord, *fakeDiscordAPI) {
	api := &fakeDiscordAPI{}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	endpoint := discordgo.EndpointChannels
	discordgo.EndpointChannels = server.URL + "/channels/"
	t.Cleanup(func() { discordgo.EndpointChannels = endpoint })

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	br := bridge.New(&config.Bridge{Account: "discord.test"})
	br.Config = config.NewConfigFromString(logger, []byte("[discord.test]\n"+cfg))
	br.Log = logrus.NewEntry(logger)

	b := New(&bridge.Config{Bridge: br}).(*Bdiscord)
	session, err := discordgo.New("Bot test")
	require.NoError(t, err)
	b.c = session
	b.channels = []*discordgo.Channel{{ID: testChannelID, Name: "general", Type: discordgo.ChannelTypeGuildText}}
	return b, api
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

			sent := api.last(t)
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
