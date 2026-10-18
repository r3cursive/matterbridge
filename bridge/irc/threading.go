package birc

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/lrstanley/girc"
	"github.com/matterbridge-org/matterbridge/bridge/config"
)

// IRCv3 support for PreserveThreading.
//
// IRC has no message IDs of its own, but servers with the message-tags capability give
// each message a msgid tag and relay the +draft/reply client tag, which clients such as
// IRCCloud use to show replies as threads. See
// https://ircv3.net/specs/extensions/message-ids and
// https://ircv3.net/specs/client-tags/reply
//
// Messages are sent through a throttled queue, so Send() can't wait for the server to
// assign a msgid. Instead Send() returns a local ID straight away, and when the server
// echoes the line back (echo-message) the local ID is mapped to the real msgid. With
// labeled-response the line carries a label and its echo is recognised by that, so
// servers changing the text (colour stripping, censoring) don't matter. Without it,
// echoes are matched by channel and text.

const (
	capEchoMessage     = "echo-message"
	capLabeledResponse = "labeled-response"
	capMessageTags     = "message-tags"
	tagLabel           = "label"
	tagMsgID           = "msgid"
	tagReply           = "+draft/reply"
	tagReplyFinal      = "+reply" // name the tag will have once ratified, some clients send both
	localIDPrefix      = "mbirc-"

	threadCacheSize   = 5000
	maxPendingEchoes  = 256
	pendingEchoExpiry = time.Minute
)

// ircCaps is what a connection negotiated for threading. It is followed from the CAP and
// ISUPPORT lines rather than asked of girc, because Send() must not wait on girc's client
// lock, which is held while dialing.
type ircCaps struct {
	echo, labels, tags, replyDenied atomic.Bool
}

// canTrackEchoes reports whether we can learn the msgid of messages we send.
func (c *ircCaps) canTrackEchoes() bool {
	return c != nil && c.tags.Load() && c.echo.Load()
}

// canTagReplies reports whether the server will relay +draft/reply for us.
func (c *ircCaps) canTagReplies() bool {
	return c != nil && c.tags.Load() && !c.replyDenied.Load()
}

func (c *ircCaps) update(event girc.Event) {
	switch event.Command {
	case girc.CAP:
		if len(event.Params) != 3 {
			return
		}
		switch event.Params[1] {
		case girc.CAP_ACK:
			for _, name := range strings.Fields(event.Last()) {
				c.set(strings.TrimPrefix(name, "-"), !strings.HasPrefix(name, "-"))
			}
		case girc.CAP_DEL:
			for _, name := range strings.Fields(event.Last()) {
				c.set(name, false)
			}
		}
	case girc.RPL_ISUPPORT:
		for _, token := range event.Params {
			switch name, value, _ := strings.Cut(token, "="); name {
			case "CLIENTTAGDENY":
				c.replyDenied.Store(clientTagDenied(value, strings.TrimPrefix(tagReply, "+")))
			case "-CLIENTTAGDENY":
				c.replyDenied.Store(false)
			}
		}
	case girc.DISCONNECTED:
		c.echo.Store(false)
		c.labels.Store(false)
		c.tags.Store(false)
		c.replyDenied.Store(false)
	}
}

func (c *ircCaps) set(name string, enabled bool) {
	name, _, _ = strings.Cut(name, "=")
	switch name {
	case capEchoMessage:
		c.echo.Store(enabled)
	case capLabeledResponse:
		c.labels.Store(enabled)
	case capMessageTags:
		c.tags.Store(enabled)
	}
}

// clientTagDenied checks a CLIENTTAGDENY ISUPPORT value for the given client tag (without the + prefix).
// See https://ircv3.net/specs/extensions/message-tags#rpl_isupport-tokens
func clientTagDenied(clientTagDeny, tag string) bool {
	if clientTagDeny == "" {
		return false
	}
	entries := strings.Split(clientTagDeny, ",")
	denyAll := false
	for _, e := range entries {
		switch e {
		case "*":
			denyAll = true
		case tag:
			return true
		}
	}
	if denyAll {
		for _, e := range entries {
			if e == "-"+tag {
				return false
			}
		}
		return true
	}
	return false
}

// validMsgID reports whether a msgid can be put in a tag as it is. girc parses the raw
// lines doSend builds and writes tag values back unescaped, so anything that would need
// escaping is refused.
func validMsgID(id string) bool {
	if id == "" {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] > '~' || id[i] == ';' || id[i] == '\\' {
			return false
		}
	}
	return true
}

type pendingEcho struct {
	label   string // with labeled-response
	channel string
	text    string
	localID string // empty for lines only tracked to keep echoes in order
	queued  time.Time
}

// ircThreads tracks the msgids of messages on IRC, and which one belongs to each ID handed to the gateway.
type ircThreads struct {
	caps atomic.Pointer[ircCaps] // of the current connection, nil without PreserveThreading

	mu      sync.Mutex
	pending []pendingEcho
	now     func() time.Time

	channels     *lru.Cache[string, string] // msgid to the channel it was seen in
	localToMsgID *lru.Cache[string, string]
	msgIDToLocal *lru.Cache[string, string]
}

func newIRCThreads() *ircThreads {
	channels, _ := lru.New[string, string](threadCacheSize)
	localToMsgID, _ := lru.New[string, string](threadCacheSize)
	msgIDToLocal, _ := lru.New[string, string](threadCacheSize)
	return &ircThreads{
		now:          time.Now,
		channels:     channels,
		localToMsgID: localToMsgID,
		msgIDToLocal: msgIDToLocal,
	}
}

func newLocalID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return localIDPrefix + hex.EncodeToString(b)
}

// expect records a line we're about to send, so its echo can be recognised.
func (t *ircThreads) expect(p pendingEcho) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p.queued = t.now()
	if len(t.pending) >= maxPendingEchoes {
		t.pending = t.pending[1:]
	}
	t.pending = append(t.pending, p)
}

// echoed handles the server echoing a line we sent, mapping its msgid to the local ID it was sent under.
// Echoes are matched by label when there is one. Otherwise by channel and text: lines are echoed in
// the order they were sent, so unlabeled lines to that channel still pending before the match were lost.
func (t *ircThreads) echoed(label, channel, text, msgID string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cutoff := t.now().Add(-pendingEchoExpiry)
	match := -1
	for i, p := range t.pending {
		if !p.queued.After(cutoff) {
			continue
		}
		byLabel := label != "" && p.label == label
		byText := label == "" && p.label == "" && text != "" && strings.EqualFold(p.channel, channel) && strings.HasPrefix(p.text, text)
		if byLabel || byText {
			match = i
			break
		}
	}
	if match == -1 {
		return "", false
	}

	matched := t.pending[match]
	kept := t.pending[:0]
	for i, p := range t.pending {
		lost := i < match && label == "" && p.label == "" && strings.EqualFold(p.channel, channel)
		if i != match && !lost && p.queued.After(cutoff) {
			kept = append(kept, p)
		}
	}
	t.pending = kept

	if matched.localID == "" {
		return "", false
	}
	t.channels.Add(msgID, matched.channel)
	t.localToMsgID.Add(matched.localID, msgID)
	t.msgIDToLocal.Add(msgID, matched.localID)
	return matched.localID, true
}

// seen records the msgid of a message from IRC.
func (t *ircThreads) seen(msgID, channel string) {
	t.channels.Add(msgID, channel)
}

// replyTarget returns the msgid to put in a +draft/reply tag for a gateway parent ID, or "" if unknown.
// The parent is either a local ID we returned from Send(), or the msgid of a message that came from IRC.
// When the parent never reached IRC the gateway hands us its ID on another bridge instead, so only
// msgids seen in this channel are replied to.
func (t *ircThreads) replyTarget(parentID, channel string) string {
	msgID := parentID
	if strings.HasPrefix(parentID, localIDPrefix) {
		var ok bool
		if msgID, ok = t.localToMsgID.Get(parentID); !ok {
			return ""
		}
	}
	if seenIn, ok := t.channels.Get(msgID); !ok || !strings.EqualFold(seenIn, channel) {
		return ""
	}
	return msgID
}

// gatewayID returns the ID the gateway knows an IRC message by: the local ID if we sent it, otherwise its msgid.
func (t *ircThreads) gatewayID(msgID string) string {
	if localID, ok := t.msgIDToLocal.Get(msgID); ok {
		return localID
	}
	return msgID
}

// threadingEnabled reports whether PreserveThreading is on for this IRC account.
func (b *Birc) threadingEnabled() bool {
	return b.GetBool("PreserveThreading")
}

// setupThreading prepares a new girc client for PreserveThreading. It requests what threading
// needs on top of what girc asks for anyway (message-tags among them), and follows what the
// client negotiates and the echoes of what it sends, which girc only hands to ALL_EVENTS handlers.
func (b *Birc) setupThreading(client *girc.Client) {
	if !b.threadingEnabled() {
		b.threads.caps.Store(nil)
		return
	}
	client.Config.SupportedCaps[capEchoMessage] = nil
	client.Config.SupportedCaps[capLabeledResponse] = nil

	caps := &ircCaps{}
	b.threads.caps.Store(caps)
	client.Handlers.Add(girc.ALL_EVENTS, func(_ *girc.Client, event girc.Event) {
		caps.update(event)
		if event.Command == girc.PRIVMSG || event.Command == girc.NOTICE {
			b.handleEcho(event)
		}
	})
}

// newSendID returns the ID Send() hands the gateway for a message, or "" if we can't learn
// its msgid, which replies to it would need.
func (b *Birc) newSendID() string {
	if !b.threads.caps.Load().canTrackEchoes() {
		return ""
	}
	return newLocalID()
}

// tagLine adds IRCv3 tags to a raw line doSend is about to send for msg: the reply tag when
// msg replies to a message seen on IRC, and a label on the first line of a message Send()
// returned an ID for. Without labeled-response or MessageSplit the line is remembered to match its
// echo by text.
//
// The tags go into the raw line rather than a girc.Event so that with MessageSplit the line
// is still sent with SendRawNoSplit. girc.Client.Send would split it again.
func (b *Birc) tagLine(msg *config.Message, line string) string {
	caps := b.threads.caps.Load()
	if caps == nil {
		return line
	}

	var tags []string
	if msg.ParentValid() && caps.canTagReplies() {
		if target := b.threads.replyTarget(msg.ParentID, msg.Channel); target != "" {
			tags = append(tags, tagReply+"="+target)
		}
	}

	if caps.canTrackEchoes() {
		command, rest, _ := strings.Cut(line, " ")
		_, text, _ := strings.Cut(rest, " :")
		echo := pendingEcho{channel: msg.Channel, text: strings.TrimSuffix(text, "\r\n")}
		if strings.HasPrefix(msg.ID, localIDPrefix) {
			echo.localID = msg.ID
		}
		// Without MessageSplit girc splits long lines and would copy a label to every part, but
		// labels must not be reused. Those lines are matched by text.
		useLabels := caps.labels.Load() && b.GetBool("MessageSplit")
		switch {
		case useLabels && echo.localID != "":
			echo.label = echo.localID
			tags = append(tags, tagLabel+"="+echo.label)
			b.threads.expect(echo)
		case useLabels, command == cmdRelayMsg:
			// RELAYMSG echoes come from the relayed nick, girc doesn't mark them as echoes
		default:
			b.threads.expect(echo)
		}
	}

	if len(tags) == 0 {
		return line
	}
	return "@" + strings.Join(tags, ";") + " " + line
}

// handleEcho maps the server's echo of a line we sent to the local ID Send() returned for it.
// It sees every PRIVMSG and NOTICE, the echoes of NickServ IDENTIFY included, so it must
// never log what they say.
func (b *Birc) handleEcho(event girc.Event) {
	if len(event.Params) < 2 {
		return
	}
	label, _ := event.Tags.Get(tagLabel)
	if label == "" && !event.Echo {
		return
	}
	msgID, ok := event.Tags.Get(tagMsgID)
	if !ok || !validMsgID(msgID) {
		return
	}
	if localID, ok := b.threads.echoed(label, event.Params[0], event.Last(), msgID); ok {
		b.Log.Debugf("message %s was sent to %s as msgid %s", localID, event.Params[0], msgID)
	}
}

// threadIncoming gives a message from IRC its msgid as ID, and as ParentID the gateway's ID
// for the message it replies to.
func (b *Birc) threadIncoming(rmsg *config.Message, event girc.Event) {
	if !b.threadingEnabled() {
		return
	}
	if msgID, ok := event.Tags.Get(tagMsgID); ok && validMsgID(msgID) {
		rmsg.ID = msgID
		b.threads.seen(msgID, rmsg.Channel)
	}
	if parent := replyParent(event); parent != "" {
		rmsg.ParentID = b.threads.gatewayID(parent)
	}
}

// replyParent returns the msgid an incoming message replies to, if any.
func replyParent(event girc.Event) string {
	for _, tag := range []string{tagReply, tagReplyFinal} {
		if parent, ok := event.Tags.Get(tag); ok && validMsgID(parent) {
			return parent
		}
	}
	return ""
}
