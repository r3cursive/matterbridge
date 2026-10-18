package birc

import (
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
)

func TestNickServWait(t *testing.T) {
	t.Run("no wait without NickServ", func(t *testing.T) {
		b := newTestBirc(t, `
[irc.test]
Server="127.0.0.1:6667"
Nick="bridge"
`, nil, nil)

		start := time.Now()
		b.handleNickServ()
		assert.Less(t, time.Since(start), time.Second)
		assert.True(t, b.authDone.Load())
	})

	t.Run("NickServ gets some slack", func(t *testing.T) {
		t.Parallel()
		b := newTestBirc(t, `
[irc.test]
Server="127.0.0.1:6667"
Nick="bridge"
NickServNick="NickServ"
NickServPassword="hunter2"
`, nil, nil)
		b.i = girc.New(girc.Config{Server: "127.0.0.1", Port: 6667, Nick: "bridge", User: "bridge"}) // never connected

		start := time.Now()
		b.handleNickServ()
		assert.GreaterOrEqual(t, time.Since(start), 5*time.Second)
		assert.True(t, b.authDone.Load())
	})
}
