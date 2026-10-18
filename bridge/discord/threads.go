package bdiscord

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/matterbridge-org/matterbridge/bridge/config"
)

// Discord threads are bridged as reply chains. IRCv3 has no threads of its own, clients show a
// chain of +reply tags as one (https://ircv3.net/specs/client-tags/reply). A message in a thread
// is relayed from the thread's channel as a reply to the thread's root: the message the thread
// was started from, or without one the first message relayed from the thread. A reply from
// another bridge to a message in a thread, or to the message a thread was started from, is sent
// into that thread. The bridge never creates threads. All of this needs PreserveThreading, without
// it thread messages are left to the gateway to drop as before.

const threadCacheSize = 5000

type discordThreads struct {
	parents  *lru.Cache[string, string]   // channel ID to the channel a thread is in, "" when it isn't a thread
	roots    *lru.Cache[string, string]   // thread ID to the message its replies are relayed as replies to
	messages *lru.Cache[string, string]   // message ID to its thread, for messages relayed from or sent into threads
	joined   *lru.Cache[string, struct{}] // threads we joined
	rootMu   sync.Mutex                   // so each thread gets one root
}

func newDiscordThreads() *discordThreads {
	parents, _ := lru.New[string, string](threadCacheSize)
	roots, _ := lru.New[string, string](threadCacheSize)
	messages, _ := lru.New[string, string](threadCacheSize)
	joined, _ := lru.New[string, struct{}](threadCacheSize)
	return &discordThreads{parents: parents, roots: roots, messages: messages, joined: joined}
}

// isThreadSystemMessage reports whether m is the thread name Discord posts in a channel when a thread
// is started without a message, or the copy of the message a thread was started from at its top.
func isThreadSystemMessage(m *discordgo.Message) bool {
	return m.Type == discordgo.MessageTypeThreadCreated || m.Type == discordgo.MessageTypeThreadStarterMessage
}

// isDefinite reports whether the result of an API call is worth remembering: success, or an error
// such as an unknown channel that asking again won't change.
func isDefinite(err error) bool {
	var restErr *discordgo.RESTError
	if err == nil {
		return true
	}
	if !errors.As(err, &restErr) || restErr.Response == nil {
		return false
	}
	code := restErr.Response.StatusCode
	return code >= 400 && code < 500 && code != http.StatusTooManyRequests
}

// threadParent returns the ID of the channel a thread is in, or "" if channelID isn't a thread.
func (b *Bdiscord) threadParent(channelID string) string {
	if parentID, ok := b.threads.parents.Get(channelID); ok {
		return parentID
	}
	channel, err := b.c.State.Channel(channelID)
	if err != nil {
		channel, err = b.c.Channel(channelID)
	}
	parentID := ""
	if err == nil && channel.IsThread() {
		parentID = channel.ParentID
	}
	if isDefinite(err) {
		b.threads.parents.Add(channelID, parentID)
	}
	return parentID
}

// getThreadChannel returns the ID and name of the channel a thread is in, or "" for both if
// channelID isn't a thread in a channel we know.
func (b *Bdiscord) getThreadChannel(channelID string) (string, string) {
	if !b.GetBool("PreserveThreading") {
		return "", ""
	}
	parentID := b.threadParent(channelID)
	if parentID == "" {
		return "", ""
	}
	name := b.getChannelName(parentID)
	if name == "" {
		return "", ""
	}
	return parentID, name
}

// getChannelOrThreadName returns the name of a channel, or for a thread the name of its channel.
func (b *Bdiscord) getChannelOrThreadName(channelID string) string {
	if name := b.getChannelName(channelID); name != "" {
		return name
	}
	_, name := b.getThreadChannel(channelID)
	return name
}

// handleThreadMessage makes a message relayed from a thread a reply to the thread's root,
// unless it replies to another message in the thread.
func (b *Bdiscord) handleThreadMessage(rmsg *config.Message, threadID, parentID string) {
	b.threads.messages.Add(rmsg.ID, threadID)
	if rmsg.ParentID == "" {
		rmsg.ParentID = b.threadRoot(threadID, parentID, rmsg.ID)
	}
}

// threadRoot returns the ID of the message messageID replies to as part of a thread: the message
// the thread was started from, which has the thread's ID, or without one the first message
// relayed from the thread. It returns "" for that first message.
//
// A thread started without a message has its name posted in its channel with the thread's ID too.
// That is a system message, never relayed, so it doesn't count.
func (b *Bdiscord) threadRoot(threadID, parentID, messageID string) string {
	b.threads.rootMu.Lock()
	defer b.threads.rootMu.Unlock()

	root, ok := b.threads.roots.Get(threadID)
	if !ok {
		starter, err := b.c.ChannelMessage(parentID, threadID)
		switch {
		case err == nil && !isThreadSystemMessage(starter):
			root = threadID
		case err == nil, isDefinite(err):
			root = messageID
		default:
			b.Log.Warnf("Could not look up the message thread %s was started from: %s", threadID, err)
			return ""
		}
		b.threads.roots.Add(threadID, root)
	}
	if root == messageID {
		return ""
	}
	return root
}

// threadTarget returns the thread in channelID a message from another bridge goes into: the
// thread of the message it edits or deletes, or for a reply, the thread its parent is in or was
// started from. It returns "" for messages that don't go into a thread.
func (b *Bdiscord) threadTarget(msg *config.Message, channelID string) string {
	var threadID string
	switch {
	case !b.GetBool("PreserveThreading"):
		return ""
	case msg.ID != "":
		first, _, _ := strings.Cut(msg.ID, ";")
		threadID, _ = b.threads.messages.Get(first)
	case msg.ParentValid():
		var ok bool
		if threadID, ok = b.threads.messages.Get(msg.ParentID); !ok {
			threadID = msg.ParentID // a thread started from a message has the message's ID
		}
	}
	if threadID == "" || b.threadParent(threadID) != channelID {
		return ""
	}
	return threadID
}

// sendToThread sends msg into a thread of channelID as Send would to the channel, with no
// reference to the message it replies to: the thread is the reply chain. Webhooks keep the
// sender's name and avatar. When the thread can't be sent to, the message goes to the channel.
func (b *Bdiscord) sendToThread(msg config.Message, channelID, threadID string) (string, error) {
	msg.ParentID = ""
	useWebhooks := b.shouldMessageUseWebhooks(&msg) && msg.Event != config.EventMsgDelete
	send := func(threadID string) (string, error) {
		m := msg
		switch {
		case useWebhooks:
			return b.handleEventWebhook(&m, channelID, threadID)
		case threadID != "":
			return b.handleEventBotUser(&m, threadID)
		default:
			return b.handleEventBotUser(&m, channelID)
		}
	}

	id, err := send(threadID)
	if err != nil && msg.ID == "" {
		b.Log.Warnf("Could not send to thread %s, sending to the channel instead: %s", threadID, err)
		return send("")
	}
	for _, part := range strings.Split(id, ";") {
		if part != "" {
			b.threads.messages.Add(part, threadID)
		}
	}
	return id, err
}

// threadCreate joins new threads in bridged channels. The bot only gets the messages of
// private threads it is a member of.
func (b *Bdiscord) threadCreate(s *discordgo.Session, t *discordgo.ThreadCreate) {
	if t.GuildID != b.guildID || !b.GetBool("PreserveThreading") || b.getChannelName(t.ParentID) == "" {
		return
	}
	b.threads.parents.Add(t.ID, t.ParentID)
	// Being added to a thread brings another THREAD_CREATE
	if found, _ := b.threads.joined.ContainsOrAdd(t.ID, struct{}{}); found {
		return
	}
	if err := s.ThreadJoin(t.ID); err != nil {
		b.Log.Warnf("Could not join thread %s: %s", t.ID, err)
	}
}
