package birc

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matterbridge-org/matterbridge/bridge"
	"github.com/matterbridge-org/matterbridge/bridge/config"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

const testChannel = "#bridge"

func init() {
	// Reconnecting is left to the tests, with no minute-long waits. Set once here, as the
	// reconnect loops of every bridge in the test binary read it.
	reconnectDelay = time.Second
}

// testConfig is a config.Config with its own viper instance. config.NewConfigFromString
// uses viper's global instance, so bridges created with it in one test binary would share,
// and race on, their configuration.
type testConfig struct {
	sync.RWMutex
	v *viper.Viper
}

func newTestConfig(t *testing.T, input string) *testConfig {
	v := viper.New()
	v.SetConfigType("toml")
	// the defaults config.NewConfigFromString sets
	v.SetDefault("General.RemoteNickFormat", "[{PROTOCOL}] <{NICK}> ")
	v.SetDefault("General.Charset", "utf-8")
	v.SetDefault("General.MessageSplit", true)
	require.NoError(t, v.ReadConfig(strings.NewReader(input)))
	return &testConfig{v: v}
}

func (c *testConfig) Viper() *viper.Viper                 { return c.v }
func (c *testConfig) BridgeValues() *config.BridgeValues  { return &config.BridgeValues{} }
func (c *testConfig) IsFilenameBlacklisted(_ string) bool { return false }

func (c *testConfig) IsKeySet(key string) bool {
	c.RLock()
	defer c.RUnlock()
	return c.v.IsSet(key)
}

func (c *testConfig) GetBool(key string) (bool, bool) {
	c.RLock()
	defer c.RUnlock()
	return c.v.GetBool(key), c.v.IsSet(key)
}

func (c *testConfig) GetInt(key string) (int, bool) {
	c.RLock()
	defer c.RUnlock()
	return c.v.GetInt(key), c.v.IsSet(key)
}

func (c *testConfig) GetString(key string) (string, bool) {
	c.RLock()
	defer c.RUnlock()
	return c.v.GetString(key), c.v.IsSet(key)
}

func (c *testConfig) GetStringSlice(key string) ([]string, bool) {
	c.RLock()
	defer c.RUnlock()
	return c.v.GetStringSlice(key), c.v.IsSet(key)
}

func (c *testConfig) GetStringSlice2D(key string) ([][]string, bool) {
	c.RLock()
	defer c.RUnlock()
	var result [][]string
	err := c.v.UnmarshalKey(key, &result)
	return result, err == nil && c.v.IsSet(key)
}

func (c *testConfig) SetVal(key string, value any) {
	c.Lock()
	defer c.Unlock()
	c.v.Set(key, value)
}

// newTestBirc creates an IRC bridge for account irc.test from a TOML configuration.
func newTestBirc(t *testing.T, input string, logger *logrus.Logger, remote chan config.Message) *Birc {
	if logger == nil {
		logger = logrus.New()
		logger.SetLevel(logrus.WarnLevel)
	}
	br := bridge.New(&config.Bridge{Account: "irc.test"})
	br.Config = newTestConfig(t, input)
	br.Log = logrus.NewEntry(logger)
	return New(&bridge.Config{Bridge: br, Remote: remote}).(*Birc)
}

// testBridge connects a real IRC bridge to a fake server and joins testChannel.
type testBridge struct {
	*Birc
	server *fakeIRCServer
	remote chan config.Message
}

func newTestBridge(t *testing.T, server *fakeIRCServer, extraConfig string, logger *logrus.Logger) *testBridge {
	cfg := fmt.Sprintf(`
[irc.test]
Server=%q
Nick="bridge"
MessageDelay=1
%s
`, server.addr(), extraConfig)
	remote := make(chan config.Message, 100)
	tb := &testBridge{newTestBirc(t, cfg, logger, remote), server, remote}
	tb.connect(t)
	t.Cleanup(func() { tb.Disconnect() })
	return tb
}

func (tb *testBridge) connect(t *testing.T) {
	require.NoError(t, tb.Connect())
	require.NoError(t, tb.JoinChannel(config.ChannelInfo{Name: testChannel}))
	// Send() drops messages until the bridge has joined a channel
	require.Eventually(t, tb.prefixDone.Load, 10*time.Second, 5*time.Millisecond, "bridge never joined")
}

func (tb *testBridge) send(t *testing.T, text, parentID string) string {
	return tb.sendMsg(t, config.Message{Text: text, Username: "<WhaTheSht> ", ParentID: parentID})
}

func (tb *testBridge) sendMsg(t *testing.T, msg config.Message) string {
	msg.Channel = testChannel
	id, err := tb.Send(msg)
	require.NoError(t, err)
	return id
}

func (tb *testBridge) receive(t *testing.T) config.Message {
	select {
	case msg := <-tb.remote:
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("nothing relayed to the gateway")
	}
	return config.Message{}
}

// logHook keeps every log message for inspection, and signals when one contains a given text.
type logHook struct {
	mu       sync.Mutex
	messages []string
	match    string
	matched  chan struct{}
}

func newLogHook(match string) *logHook {
	return &logHook{match: match, matched: make(chan struct{}, 10)}
}

func (h *logHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h *logHook) Fire(entry *logrus.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, entry.Message)
	if h.match != "" && strings.Contains(entry.Message, h.match) {
		h.matched <- struct{}{}
	}
	return nil
}

func (h *logHook) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.messages...)
}

func newHookedLogger(hook *logHook) *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	logger.SetLevel(logrus.DebugLevel)
	logger.AddHook(hook)
	return logger
}
