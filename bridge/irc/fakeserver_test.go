package birc

import (
	"bufio"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIRCServer is a minimal IRCv3 server for testing the IRC bridge end to end.
//
// It serves one client at a time, a new connection replaces the current one. It doesn't relay
// messages between users. It records every PRIVMSG, NOTICE and RELAYMSG the bridge sends, gives
// it a msgid, and echoes it back like a real server does with echo-message and labeled-response.
type fakeIRCServer struct {
	t           *testing.T
	ln          net.Listener
	caps        []string // as advertised, with values
	isupport    []string // RPL_ISUPPORT tokens on top of NETWORK
	stripColors bool     // strip formatting from echoes, like channel mode +S

	mu      sync.Mutex // guards everything below, and writes to the client
	client  *fakeClient
	clients int
	sent    []sentLine
	msgIDs  int
	joined  chan string
}

type fakeClient struct {
	conn     net.Conn
	number   int
	nick     string
	gotUser  bool
	capOpen  bool
	welcomed bool
	enabled  map[string]bool
}

type sentLine struct {
	conn    int // the connection it was sent on, counting from 1
	command string
	tags    map[string]string
	target  string
	nick    string // the relayed nick, for RELAYMSG
	text    string
	msgID   string
}

func newFakeIRCServer(t *testing.T, caps []string, isupport []string) *fakeIRCServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}
	s := &fakeIRCServer{
		t:        t,
		ln:       ln,
		caps:     caps,
		isupport: isupport,
		joined:   make(chan string, 10),
	}
	go s.serve()
	t.Cleanup(s.close)
	return s
}

func (s *fakeIRCServer) addr() string { return s.ln.Addr().String() }

func (s *fakeIRCServer) close() {
	s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		s.client.conn.Close()
	}
}

func (s *fakeIRCServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.client != nil {
			s.client.conn.Close()
		}
		s.clients++
		c := &fakeClient{conn: conn, number: s.clients, nick: "*", enabled: map[string]bool{}}
		s.client = c
		s.mu.Unlock()

		go func() {
			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				s.handle(c, scanner.Text())
			}
		}()
	}
}

// writeLocked sends a line to c. s.mu must be held.
func (s *fakeIRCServer) writeLocked(c *fakeClient, format string, a ...any) {
	fmt.Fprintf(c.conn, format+"\r\n", a...)
}

func parseLine(line string) (tags map[string]string, command string, params []string) {
	tags = map[string]string{}
	if strings.HasPrefix(line, "@") {
		rawTags, rest, _ := strings.Cut(line[1:], " ")
		for _, tag := range strings.Split(rawTags, ";") {
			k, v, _ := strings.Cut(tag, "=")
			tags[k] = v
		}
		line = rest
	}
	if strings.HasPrefix(line, ":") {
		_, line, _ = strings.Cut(line, " ")
	}
	line, trailing, hasTrailing := strings.Cut(line, " :")
	fields := strings.Fields(line)
	command = strings.ToUpper(fields[0])
	params = fields[1:]
	if hasTrailing {
		params = append(params, trailing)
	}
	return tags, command, params
}

func capName(c string) string {
	name, _, _ := strings.Cut(c, "=")
	return name
}

var formatting = regexp.MustCompile("\x03[0-9]{1,2}(,[0-9]{1,2})?|[\x02\x0f\x16\x1d\x1f]")

func (s *fakeIRCServer) handle(c *fakeClient, line string) {
	tags, command, params := parseLine(line)

	s.mu.Lock()
	defer s.mu.Unlock()

	switch command {
	case "CAP":
		s.handleCapLocked(c, params)
	case "NICK":
		c.nick = params[0]
		s.tryWelcomeLocked(c)
	case "USER":
		c.gotUser = true
		s.tryWelcomeLocked(c)
	case "PING":
		s.writeLocked(c, ":fake.irc PONG fake.irc :%s", strings.Join(params, " "))
	case "JOIN":
		for _, channel := range strings.Split(params[0], ",") {
			s.writeLocked(c, ":%s!bridge@bridge.host JOIN %s", c.nick, channel)
			s.writeLocked(c, ":fake.irc 353 %s = %s :%s", c.nick, channel, c.nick)
			s.writeLocked(c, ":fake.irc 366 %s %s :End of /NAMES list.", c.nick, channel)
			select {
			case s.joined <- channel:
			default:
			}
		}
	case "WHO":
		s.writeLocked(c, ":fake.irc 315 %s %s :End of /WHO list.", c.nick, params[0])
	case "QUIT":
		c.conn.Close()
	case "PRIVMSG", "NOTICE", "RELAYMSG":
		s.handleMessageLocked(c, tags, command, params)
	}
}

func (s *fakeIRCServer) handleMessageLocked(c *fakeClient, tags map[string]string, command string, params []string) {
	s.msgIDs++
	sent := sentLine{conn: c.number, command: command, tags: tags, target: params[0], text: params[len(params)-1]}
	sent.msgID = fmt.Sprintf("msgid%d", s.msgIDs)
	if command == "RELAYMSG" {
		sent.nick = params[1]
	}
	s.sent = append(s.sent, sent)

	// Relayed messages reach every channel member from the relayed nick, the sender included.
	// The sender's other messages only come back with echo-message.
	source, echoTags := c.nick+"!bridge@bridge.host", []string{"msgid=" + sent.msgID}
	if sent.nick != "" {
		source, command = sent.nick+"!relay@relay.host", "PRIVMSG"
		echoTags = append(echoTags, "draft/relaymsg="+c.nick)
	} else if !c.enabled["echo-message"] {
		return
	}
	for k, v := range tags {
		if strings.HasPrefix(k, "+") || (k == "label" && c.enabled["labeled-response"]) {
			echoTags = append(echoTags, k+"="+v)
		}
	}
	text := sent.text
	if s.stripColors {
		text = formatting.ReplaceAllString(text, "")
	}
	s.writeLocked(c, "@%s :%s %s %s :%s", strings.Join(echoTags, ";"), source, command, sent.target, text)
}

func (s *fakeIRCServer) handleCapLocked(c *fakeClient, params []string) {
	switch strings.ToUpper(params[0]) {
	case "LS":
		if !c.welcomed {
			c.capOpen = true
		}
		s.writeLocked(c, ":fake.irc CAP %s LS :%s", c.nick, strings.Join(s.caps, " "))
	case "REQ":
		requested := strings.Fields(params[1])
		for _, r := range requested {
			if !slices.ContainsFunc(s.caps, func(c string) bool { return capName(c) == r }) {
				s.writeLocked(c, ":fake.irc CAP %s NAK :%s", c.nick, params[1])
				return
			}
		}
		for _, r := range requested {
			c.enabled[r] = true
		}
		s.writeLocked(c, ":fake.irc CAP %s ACK :%s", c.nick, strings.Join(requested, " "))
	case "END":
		c.capOpen = false
		s.tryWelcomeLocked(c)
	}
}

// tryWelcomeLocked completes registration once NICK and USER are in and CAP negotiation is over.
func (s *fakeIRCServer) tryWelcomeLocked(c *fakeClient) {
	if c.welcomed || c.nick == "*" || !c.gotUser || c.capOpen {
		return
	}
	c.welcomed = true
	s.writeLocked(c, ":fake.irc 001 %s :Welcome", c.nick)
	s.writeLocked(c, ":fake.irc 005 %s %s :are supported by this server", c.nick, strings.Join(append([]string{"NETWORK=Fake"}, s.isupport...), " "))
	s.writeLocked(c, ":fake.irc 422 %s :MOTD File is missing", c.nick)
}

// capEnabled reports whether the current client negotiated a capability.
func (s *fakeIRCServer) capEnabled(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client != nil && s.client.enabled[name]
}

// sendFromUser sends a channel message to the bridge from another user and returns its msgid.
func (s *fakeIRCServer) sendFromUser(nick, channel, text string, extraTags map[string]string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgIDs++
	msgID := fmt.Sprintf("msgid%d", s.msgIDs)
	prefix := ""
	if s.client.enabled["message-tags"] {
		tags := []string{"msgid=" + msgID}
		for k, v := range extraTags {
			tags = append(tags, k+"="+v)
		}
		prefix = "@" + strings.Join(tags, ";") + " "
	}
	s.writeLocked(s.client, "%s:%s!user@user.host PRIVMSG %s :%s", prefix, nick, channel, text)
	return msgID
}

// drop closes the connection to the client, as a network failure would.
func (s *fakeIRCServer) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client.conn.Close()
}

// quit sends the client its own QUIT with reason and drops the connection.
func (s *fakeIRCServer) quit(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeLocked(s.client, ":%s!bridge@bridge.host QUIT :%s", s.client.nick, reason)
	s.client.conn.Close()
}

func (s *fakeIRCServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients
}

// awaitSent waits until the bridge has sent at least n lines, and returns them.
func (s *fakeIRCServer) awaitSent(n int) []sentLine {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.sent) >= n {
			out := slices.Clone(s.sent)
			s.mu.Unlock()
			return out
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t.Fatalf("timed out waiting for %d sent lines, got %d: %#v", n, len(s.sent), s.sent)
	return nil
}

func (s *fakeIRCServer) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}
