package birc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/matterbridge-org/matterbridge/bridge/config"
	"github.com/matterbridge-org/matterbridge/bridge/helper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var allCaps = []string{capMessageTags, capEchoMessage, capLabeledResponse, "batch", "server-time"}

func TestClientTagDenied(t *testing.T) {
	for _, tc := range []struct {
		deny   string
		denied bool
	}{
		{"", false},
		{"typing", false},
		{"typing,draft/reply", true},
		{"*", true},
		{"*,-draft/reply", false},
		{"*,-draft/typing,-typing,-draft/channel-context,-draft/reply,-reply", false}, // UnrealIRCd 6 default
	} {
		assert.Equal(t, tc.denied, clientTagDenied(tc.deny, "draft/reply"), "CLIENTTAGDENY=%q", tc.deny)
	}
}

func TestValidMsgID(t *testing.T) {
	for _, id := range []string{"msgid1", "AbC-_9xyz", "1697654321:abc"} {
		assert.True(t, validMsgID(id), id)
	}
	for _, id := range []string{"", "with space", "semi;colon", `back\slash`, "new\nline", "ünicode"} {
		assert.False(t, validMsgID(id), id)
	}
}

func TestCapsFollowNegotiation(t *testing.T) {
	var none *ircCaps
	assert.False(t, none.canTrackEchoes())
	assert.False(t, none.canTagReplies())

	caps := &ircCaps{}
	caps.update(girc.Event{Command: girc.CAP, Params: []string{"bridge", girc.CAP_ACK, "message-tags echo-message labeled-response batch"}})
	assert.True(t, caps.canTrackEchoes())
	assert.True(t, caps.canTagReplies())
	assert.True(t, caps.labels.Load())

	caps.update(girc.Event{Command: girc.RPL_ISUPPORT, Params: []string{"bridge", "NETWORK=Fake", "CLIENTTAGDENY=*", "are supported by this server"}})
	assert.False(t, caps.canTagReplies(), "CLIENTTAGDENY=* blocks the reply tag")
	caps.update(girc.Event{Command: girc.RPL_ISUPPORT, Params: []string{"bridge", "-CLIENTTAGDENY", "are supported by this server"}})
	assert.True(t, caps.canTagReplies())

	caps.update(girc.Event{Command: girc.CAP, Params: []string{"bridge", girc.CAP_DEL, "echo-message"}})
	assert.False(t, caps.canTrackEchoes())
	assert.True(t, caps.canTagReplies())

	caps.update(girc.Event{Command: girc.DISCONNECTED, Params: []string{"127.0.0.1:6667"}})
	assert.False(t, caps.canTagReplies())
	assert.False(t, caps.labels.Load())
}

func TestThreadsEchoMatchedByLabel(t *testing.T) {
	threads := newIRCThreads()
	threads.expect(pendingEcho{label: "mbirc-1", channel: testChannel, text: "<a> one", localID: "mbirc-1"})
	threads.expect(pendingEcho{label: "mbirc-2", channel: testChannel, text: "<b> two", localID: "mbirc-2"})

	_, ok := threads.echoed("mbirc-unknown", testChannel, "<a> one", "x")
	assert.False(t, ok)

	// labels are matched whatever the server did to the text, and in any order
	localID, ok := threads.echoed("mbirc-2", testChannel, "two", "msgid2")
	assert.True(t, ok)
	assert.Equal(t, "mbirc-2", localID)
	localID, ok = threads.echoed("mbirc-1", testChannel, "one", "msgid1")
	assert.True(t, ok)
	assert.Equal(t, "mbirc-1", localID)

	_, ok = threads.echoed("mbirc-1", testChannel, "one", "msgid1-again")
	assert.False(t, ok, "each label is matched once")
	assert.Equal(t, "msgid1", threads.replyTarget("mbirc-1", testChannel))
	assert.Equal(t, "mbirc-2", threads.gatewayID("msgid2"))
}

func TestThreadsEchoMatchedByText(t *testing.T) {
	threads := newIRCThreads()
	threads.expect(pendingEcho{channel: testChannel, text: "<a> one", localID: "mbirc-1"})
	threads.expect(pendingEcho{channel: "#other", text: "<c> elsewhere", localID: "mbirc-3"})
	threads.expect(pendingEcho{channel: testChannel, text: "<b> second line"}) // not tracked, keeps the order
	threads.expect(pendingEcho{channel: testChannel, text: "<b> two", localID: "mbirc-2"})

	// lines nobody is waiting on, and echoes without a label when expecting labels, are ignored
	_, ok := threads.echoed("", testChannel, "<z> unrelated", "x")
	assert.False(t, ok)

	// "<a> one" and the untracked line were never echoed, matching "<b> two" drops them but keeps other channels
	localID, ok := threads.echoed("", testChannel, "<b> two", "msgid2")
	assert.True(t, ok)
	assert.Equal(t, "mbirc-2", localID)
	_, ok = threads.echoed("", testChannel, "<a> one", "msgid1")
	assert.False(t, ok)
	localID, ok = threads.echoed("", "#OTHER", "<c> elsewhere", "msgid3")
	assert.True(t, ok)
	assert.Equal(t, "mbirc-3", localID)

	assert.Equal(t, "msgid2", threads.replyTarget("mbirc-2", testChannel))
	assert.Equal(t, "mbirc-2", threads.gatewayID("msgid2"))
}

func TestThreadsEchoOfSplitLine(t *testing.T) {
	threads := newIRCThreads()
	threads.expect(pendingEcho{channel: testChannel, text: "<a> a long line that girc split", localID: "mbirc-1"})

	localID, ok := threads.echoed("", testChannel, "<a> a long line", "msgid1")
	assert.True(t, ok)
	assert.Equal(t, "mbirc-1", localID)
}

func TestThreadsEchoExpires(t *testing.T) {
	threads := newIRCThreads()
	now := time.Now()
	threads.now = func() time.Time { return now }
	threads.expect(pendingEcho{label: "mbirc-1", channel: testChannel, text: "<a> slow", localID: "mbirc-1"})
	threads.expect(pendingEcho{channel: testChannel, text: "<a> slow", localID: "mbirc-2"})

	now = now.Add(2 * pendingEchoExpiry)
	_, ok := threads.echoed("mbirc-1", testChannel, "<a> slow", "msgid1")
	assert.False(t, ok)
	_, ok = threads.echoed("", testChannel, "<a> slow", "msgid2")
	assert.False(t, ok)
}

func TestThreadsReplyTarget(t *testing.T) {
	threads := newIRCThreads()
	threads.seen("msgidX", testChannel)

	assert.Equal(t, "msgidX", threads.replyTarget("msgidX", testChannel), "msgids seen on IRC can be replied to")
	assert.Equal(t, "msgidX", threads.replyTarget("msgidX", "#BRIDGE"))
	assert.Equal(t, "", threads.replyTarget("msgidX", "#other"), "but only in the channel they were seen in")
	assert.Equal(t, "", threads.replyTarget("msgidY", testChannel), "unknown msgids may come from another network")
	assert.Equal(t, "", threads.replyTarget("discord 1234567890", testChannel), "the gateway falls back to other bridges' IDs")
	assert.Equal(t, "", threads.replyTarget(localIDPrefix+"unknown", testChannel), "local IDs never echoed can't be replied to")
	assert.Equal(t, "msgidX", threads.gatewayID("msgidX"))
}

// awaitMsgID waits for the echo of a message we sent to be mapped to its msgid.
func (tb *testBridge) awaitMsgID(t *testing.T, localID string) string {
	var msgID string
	require.Eventually(t, func() bool {
		var ok bool
		msgID, ok = tb.threads.localToMsgID.Get(localID)
		return ok
	}, 10*time.Second, 5*time.Millisecond, "echo of %s was never mapped", localID)
	return msgID
}

func TestThreadingWithFullSupport(t *testing.T) {
	t.Parallel()
	server := newFakeIRCServer(t, allCaps, nil)
	tb := newTestBridge(t, server, "PreserveThreading=true", nil)

	t.Run("echoes keep reaching the threading handler once connected", func(t *testing.T) {
		// girc only hands echoes to ALL_EVENTS handlers, handleOther is the one to go
		require.Eventually(t, func() bool { return tb.i.Handlers.Count(girc.ALL_EVENTS) == 1 }, 10*time.Second, 5*time.Millisecond)
	})

	t.Run("reply to a message from discord is threaded", func(t *testing.T) {
		before := server.sentCount()
		localID := tb.send(t, "kick attempt is dumb", "")
		require.True(t, strings.HasPrefix(localID, localIDPrefix), "Send should return an ID when echoes can be tracked")
		msgID := tb.awaitMsgID(t, localID)
		first := server.awaitSent(before + 1)[before]
		assert.Equal(t, first.msgID, msgID)
		assert.Equal(t, localID, first.tags[tagLabel], "the echo is recognised by its label")

		tb.send(t, "hilarious hearing this from a pats fan", localID)
		reply := server.awaitSent(before + 2)[before+1]
		assert.Equal(t, "<WhaTheSht> hilarious hearing this from a pats fan", reply.text)
		assert.Equal(t, msgID, reply.tags[tagReply])
	})

	t.Run("reply to a message from irc is threaded", func(t *testing.T) {
		msgID := server.sendFromUser("skud", testChannel, "lmao broadcast camera follows the fake", nil)
		received := tb.receive(t)
		assert.Equal(t, msgID, received.ID, "IRC messages should carry their msgid to the gateway")

		before := server.sentCount()
		tb.send(t, "roasted", msgID)
		assert.Equal(t, msgID, server.awaitSent(before + 1)[before].tags[tagReply])
	})

	t.Run("irc replies carry their parent to the gateway", func(t *testing.T) {
		// parent is a message that came from IRC: the gateway knows it by msgid
		parentMsgID := server.sendFromUser("skud", testChannel, "first", nil)
		tb.receive(t)
		server.sendFromUser("RustyCloud", testChannel, "reply to first", map[string]string{tagReply: parentMsgID})
		assert.Equal(t, parentMsgID, tb.receive(t).ParentID)

		// parent is a message we sent: the gateway knows it by the ID Send() returned
		localID := tb.send(t, "from discord", "")
		ourMsgID := tb.awaitMsgID(t, localID)
		server.sendFromUser("RustyCloud", testChannel, "reply to discord", map[string]string{tagReply: ourMsgID})
		assert.Equal(t, localID, tb.receive(t).ParentID)
	})

	t.Run("parents not seen in the channel are sent unthreaded", func(t *testing.T) {
		require.NoError(t, tb.JoinChannel(config.ChannelInfo{Name: "#other"}))
		otherMsgID := server.sendFromUser("skud", "#other", "in another channel", nil)
		assert.Equal(t, "#other", tb.receive(t).Channel)

		before := server.sentCount()
		parents := []string{
			config.ParentIDNotFound,
			localIDPrefix + "neverseen",
			"discord 1234567890", // the gateway's fallback when the parent never reached IRC
			"msgid-from-another-network",
			otherMsgID,
		}
		for _, parent := range parents {
			tb.send(t, "reply to "+parent, parent)
		}
		for _, line := range server.awaitSent(before + len(parents))[before:] {
			assert.NotContains(t, line.tags, tagReply, line.text)
		}
	})

	t.Run("every line of a multi-line reply is threaded", func(t *testing.T) {
		before := server.sentCount()
		localID := tb.send(t, "first", "")
		msgID := tb.awaitMsgID(t, localID)

		replyID := tb.send(t, "line one\nline two\nline three", localID)
		lines := server.awaitSent(before + 4)[before+1:]
		assert.Equal(t, []string{"<WhaTheSht> line one", "<WhaTheSht> line two", "<WhaTheSht> line three"},
			[]string{lines[0].text, lines[1].text, lines[2].text})
		for _, line := range lines {
			assert.Equal(t, msgID, line.tags[tagReply], line.text)
		}
		assert.Equal(t, replyID, lines[0].tags[tagLabel], "the first line is the one Send() returns an ID for")
		assert.NotContains(t, lines[1].tags, tagLabel)
		assert.NotContains(t, lines[2].tags, tagLabel)
	})

	t.Run("long replies are split by the bridge, not girc, and every line is threaded", func(t *testing.T) {
		before := server.sentCount()
		localID := tb.send(t, "first", "")
		msgID := tb.awaitMsgID(t, localID)

		msg := config.Message{Text: strings.Repeat("lorem ipsum dolor sit amet ", 60), Username: "<WhaTheSht> ", Channel: testChannel}
		prefix := tb.handlePrefix(&msg)
		want := helper.GetSubLinesWords(msg.Text, int(tb.MessageLength.Load())-prefix, "")
		require.Greater(t, len(want), 2)

		msg.ParentID = localID
		tb.sendMsg(t, msg)
		tb.send(t, "next message", "")
		lines := server.awaitSent(before + 1 + len(want) + 1)[before+1:]
		for i, text := range want {
			assert.Equal(t, "<WhaTheSht> "+text, lines[i].text)
			assert.Equal(t, msgID, lines[i].tags[tagReply])
		}
		assert.Equal(t, "<WhaTheSht> next message", lines[len(want)].text, "girc split the lines again")
		assert.Greater(t, len("PRIVMSG "+testChannel+" :"+lines[0].text), tb.i.MaxEventLength(), "a line girc would split")
	})

	t.Run("actions and notices are threaded", func(t *testing.T) {
		before := server.sentCount()
		localID := tb.send(t, "first", "")
		msgID := tb.awaitMsgID(t, localID)

		tb.sendMsg(t, config.Message{Text: "facepalms", Username: "WhaTheSht ", ParentID: localID, Event: config.EventUserAction})
		tb.sendMsg(t, config.Message{Text: "noted", Username: "<WhaTheSht> ", ParentID: localID, Event: config.EventNoticeIRC})
		lines := server.awaitSent(before + 3)
		assert.Equal(t, "\x01ACTION WhaTheSht facepalms\x01", lines[before+1].text)
		assert.Equal(t, msgID, lines[before+1].tags[tagReply])
		assert.Equal(t, "NOTICE", lines[before+2].command)
		assert.Equal(t, msgID, lines[before+2].tags[tagReply])
	})
}

func TestThreadingEchoesWithChangedText(t *testing.T) {
	t.Parallel()
	server := newFakeIRCServer(t, allCaps, nil)
	server.stripColors = true // like channel mode +S
	tb := newTestBridge(t, server, "PreserveThreading=true\nColorNicks=true", nil)

	localID := tb.send(t, "colourful", "")
	msgID := tb.awaitMsgID(t, localID)
	tb.send(t, "reply", localID)
	assert.Equal(t, msgID, server.awaitSent(2)[1].tags[tagReply])
}

func TestThreadingWithoutMessageSplit(t *testing.T) {
	t.Parallel()
	server := newFakeIRCServer(t, allCaps, nil)
	tb := newTestBridge(t, server, "PreserveThreading=true\nMessageSplit=false", nil)

	localID := tb.send(t, "first", "")
	msgID := tb.awaitMsgID(t, localID)
	// without MessageSplit girc's flood control is on, and would hold the next lines back for seconds
	time.Sleep(time.Second)

	// girc splits the line, and copies the tags to every part
	replyID := tb.send(t, strings.Repeat("lorem ipsum dolor sit amet ", 18), localID)
	tb.send(t, "next message", "")
	require.Eventually(t, func() bool {
		lines := server.awaitSent(1)
		return lines[len(lines)-1].text == "<WhaTheSht> next message"
	}, 10*time.Second, 5*time.Millisecond)
	lines := server.awaitSent(1)
	parts := lines[1 : len(lines)-1]
	require.Greater(t, len(parts), 1)
	for _, part := range parts {
		assert.Equal(t, msgID, part.tags[tagReply], part.text)
		assert.NotContains(t, part.tags, tagLabel, "a label copied to every part would be reused")
	}

	// echoes are matched by text instead
	assert.Equal(t, parts[0].msgID, tb.awaitMsgID(t, replyID), "the reply is known by the msgid of its first part")
}

func TestThreadingWithRelayMsg(t *testing.T) {
	for _, split := range []bool{true, false} {
		t.Run(fmt.Sprintf("MessageSplit=%t", split), func(t *testing.T) {
			t.Parallel()
			server := newFakeIRCServer(t, append([]string{"draft/relaymsg=/"}, allCaps...), nil)
			tb := newTestBridge(t, server, fmt.Sprintf("PreserveThreading=true\nUseRelayMsg=true\nMessageSplit=%t", split), nil)
			require.Eventually(t, func() bool { return server.capEnabled("draft/relaymsg") }, 10*time.Second, 5*time.Millisecond)

			localID := tb.sendMsg(t, config.Message{Text: "first", Username: "WhaTheSht/discord"})
			parentID, msgID := localID, ""
			if split {
				msgID = tb.awaitMsgID(t, localID)
			} else {
				// RELAYMSG echoes come from the relayed nick, so only a label tells them apart,
				// and lines aren't labeled without MessageSplit. Replies to IRC are threaded still.
				msgID = server.sendFromUser("skud", testChannel, "from irc", nil)
				parentID = tb.receive(t).ID
			}

			tb.sendMsg(t, config.Message{Text: "reply", Username: "WhaTheSht/discord", ParentID: parentID})
			tb.sendMsg(t, config.Message{Text: "facepalms", Username: "WhaTheSht/discord", ParentID: parentID, Event: config.EventUserAction})
			lines := server.awaitSent(3)
			for _, line := range lines {
				assert.Equal(t, "RELAYMSG", line.command)
				assert.Equal(t, "WhaTheSht/discord", line.nick)
			}
			assert.Equal(t, split, lines[0].tags[tagLabel] != "", "labeled only with MessageSplit")
			assert.Equal(t, "reply", lines[1].text)
			assert.Equal(t, msgID, lines[1].tags[tagReply])
			assert.Equal(t, "\x01ACTION facepalms\x01", lines[2].text)
			assert.Equal(t, msgID, lines[2].tags[tagReply])

			// our relayed messages come back from the relayed nick and aren't passed on
			select {
			case msg := <-tb.remote:
				t.Errorf("relayed %#v to the gateway", msg)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestThreadingFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name         string
		caps         []string
		isupport     []string
		config       string
		tracksEchoes bool
		labels       bool
		sendsTags    bool
	}{
		{"PreserveThreading off", allCaps, nil, "", false, false, false},
		{"no message-tags", []string{capEchoMessage, capLabeledResponse}, nil, "PreserveThreading=true", false, false, false},
		{"no echo-message", []string{capMessageTags, capLabeledResponse}, nil, "PreserveThreading=true", false, false, true},
		{"no labeled-response", []string{capMessageTags, capEchoMessage}, nil, "PreserveThreading=true", true, false, true},
		{"reply tag denied", allCaps, []string{"CLIENTTAGDENY=*"}, "PreserveThreading=true", true, true, false},
		{"reply tag allowed by exception", allCaps, []string{"CLIENTTAGDENY=*,-draft/reply"}, "PreserveThreading=true", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := newFakeIRCServer(t, tc.caps, tc.isupport)
			tb := newTestBridge(t, server, tc.config, nil)

			id := tb.send(t, "first", "")
			assert.Equal(t, tc.tracksEchoes, id != "", "Send() returned %q", id)
			if tc.tracksEchoes {
				tb.awaitMsgID(t, id)
			}

			// replying to a message from IRC only needs the reply tag
			msgID := server.sendFromUser("skud", testChannel, "from irc", nil)
			received := tb.receive(t)
			if tc.config == "" {
				assert.Empty(t, received.ID, "msgids should only be passed on with PreserveThreading")
				assert.False(t, server.capEnabled(capEchoMessage), "echo-message should only be requested with PreserveThreading")
				assert.False(t, server.capEnabled(capLabeledResponse))
			}

			tb.send(t, "reply", msgID)
			lines := server.awaitSent(2)
			assert.Equal(t, "<WhaTheSht> first", lines[0].text)
			assert.Equal(t, "<WhaTheSht> reply", lines[1].text)
			if tc.labels {
				assert.Equal(t, id, lines[0].tags[tagLabel])
			} else {
				assert.Empty(t, lines[0].tags)
			}
			if tc.sendsTags {
				assert.Equal(t, msgID, lines[1].tags[tagReply])
			} else {
				assert.NotContains(t, lines[1].tags, tagReply)
			}
		})
	}
}

func TestThreadingAfterReconnect(t *testing.T) {
	t.Parallel()
	hook := newLogHook("reconnecting in")
	server := newFakeIRCServer(t, allCaps, nil)
	tb := newTestBridge(t, server, "PreserveThreading=true", newHookedLogger(hook))

	localID := tb.send(t, "before", "")
	tb.awaitMsgID(t, localID)

	// The server dropping us with a ping timeout makes the bridge ask the gateway to reconnect it.
	server.quit("Ping timeout: 240 seconds")
	for msg := tb.receive(t); msg.Event != config.EventFailure; msg = tb.receive(t) {
	}
	select {
	case <-hook.matched: // the old connection is done resetting the bridge's state
	case <-time.After(10 * time.Second):
		t.Fatal("old connection never finished")
	}

	// do what gateway.reconnectBridge does
	require.NoError(t, tb.Disconnect())
	tb.connect(t)
	require.Equal(t, 2, server.connections())

	before := server.sentCount()
	localID = tb.send(t, "after", "")
	msgID := tb.awaitMsgID(t, localID)
	tb.send(t, "reply", localID)
	lines := server.awaitSent(before + 2)
	assert.Equal(t, 2, lines[before+1].conn)
	assert.Equal(t, msgID, lines[before+1].tags[tagReply])
	require.Eventually(t, func() bool { return tb.i.Handlers.Count(girc.ALL_EVENTS) == 1 }, 10*time.Second, 5*time.Millisecond)
}

func TestEchoesAreNeverLogged(t *testing.T) {
	t.Parallel()
	hook := newLogHook("")
	server := newFakeIRCServer(t, allCaps, nil)
	// DebugLevel=1 keeps handleOther logging every event
	tb := newTestBridge(t, server, `PreserveThreading=true
DebugLevel=1
NickServNick="NickServ"
NickServPassword="hunter2"
RunCommands=["PRIVMSG NickServ :IDENTIFY bridge hunter2"]`, newHookedLogger(hook))

	// The IDENTIFY echoes came in before the bridge joined, and handlers run in order.
	localID := tb.send(t, "after identifying", "")
	tb.awaitMsgID(t, localID)

	lines := server.awaitSent(3)
	assert.Equal(t, "NickServ", lines[0].target)
	assert.Equal(t, "NickServ", lines[1].target)
	require.True(t, server.capEnabled(capEchoMessage), "IDENTIFY was echoed")
	for _, message := range hook.all() {
		assert.NotContains(t, message, "hunter2")
	}
}
