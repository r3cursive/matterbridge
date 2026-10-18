package birc

import (
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/matterbridge-org/matterbridge/bridge/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconnectThroughTheGateway(t *testing.T) {
	t.Parallel()
	hook := newLogHook("reconnecting in")
	server := newFakeIRCServer(t, nil, nil)
	tb := newTestBridge(t, server, "", newHookedLogger(hook))

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

	// the old connection's reconnect loop must not wake up and reconnect the new client
	time.Sleep(reconnectDelay + time.Second)
	assert.Equal(t, 2, server.connections())
	tb.send(t, "after", "")
	line := server.awaitSent(1)[0]
	assert.Equal(t, 2, line.conn)
	assert.Equal(t, "<WhaTheSht> after", line.text)

	// the new connection is set up like the first one
	require.Eventually(t, func() bool { return tb.i.Handlers.Count(girc.ALL_EVENTS) == 0 }, 10*time.Second, 5*time.Millisecond,
		"handleOther should be removed once connected")
}

func TestReconnectOnItsOwn(t *testing.T) {
	t.Parallel()
	server := newFakeIRCServer(t, nil, nil)
	tb := newTestBridge(t, server, "", nil)

	// The bridge reconnects by itself when the connection drops, and asks the gateway to rejoin
	// its channels. The fake server sends no NOTICE or PRIVMSG that would tell it its nick.
	server.drop()
	for msg := tb.receive(t); msg.Event != config.EventRejoinChannels; msg = tb.receive(t) {
	}
	require.NoError(t, tb.JoinChannel(config.ChannelInfo{Name: testChannel}))
	require.Eventually(t, tb.prefixDone.Load, 15*time.Second, 5*time.Millisecond, "the bridge didn't recognise its own JOIN")
	assert.Equal(t, "bridge", tb.getNick())

	tb.send(t, "after", "")
	line := server.awaitSent(1)[0]
	assert.Equal(t, 2, line.conn)
	assert.Equal(t, "<WhaTheSht> after", line.text)
}
