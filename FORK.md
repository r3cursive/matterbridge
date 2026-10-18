# FORK.md: patches this fork carries

This file is the ledger of every patch `r3cursive/matterbridge` carries on top of matterbridge-org.
Read it before you merge from upstream. It says what each patch does, why, and where it will conflict.

## 1. What this fork is

`r3cursive/matterbridge` is [matterbridge-org/matterbridge](https://github.com/matterbridge-org/matterbridge) plus a short patch series.
The series adds IRCv3 reply threading between IRC and Discord, and fixes bugs found on the way.

| | |
|---|---|
| Upstream | `matterbridge-org/matterbridge`, branch `master` |
| Upstream base | `3c508a6e63c00c60d0cb0f929029e6b607621c5b` ("add Stoat bridge support") |
| `master` | mirrors upstream, fast-forward only, never committed to |
| `irc-threading` | `master` plus the series below, rebased on every upstream merge |
| Deployed by | `r3cursive/dis4irc-deploy`, pinned by full SHA in its `Dockerfile` |

The reply threading design comes from RustyBower's [42wim/matterbridge PR #2261](https://github.com/42wim/matterbridge/pull/2261).
matterbridge-org rewrote both its history and the IRC send path, so the PR was ported by hand.
Commits derived from that PR credit Rusty Bower as co-author.

SHAs change on every rebase. The ledger below keys each patch by its subject.

## 2. Patch ledger

The series runs in this order. Audit IDs (H1 to H3, M1 to M6) refer to the review of the port, explained in section 3.

| # | Subject | Upstreamable alone | Conflict risk |
|---|---|---|---|
| 1 | irc: guard the bridge nick against concurrent access | yes | medium |
| 2 | irc: make connection-state flags race-safe | yes | high |
| 3 | irc: stop a disconnected client's reconnect loop | yes | medium |
| 4 | irc: take our nick from RPL_WELCOME's target on reconnect | yes, after 3 | low |
| 5 | irc: only wait for NickServ when it is configured | yes | low |
| 6 | irc: support PreserveThreading with IRCv3 reply tags | after 1 to 5 | high |
| 7 | irc: accept the +reply tag on incoming replies | with 6 | low |
| 8 | discord: only reply to snowflake parents, never fail on missing ones | yes | low |
| 9 | discord: build reply quotes from the processed message text | yes | low |
| 10 | discord: set the username apart in threaded replies sent by the bot | yes | low |
| 11 | irc: send the ratified +reply tag alongside +draft/reply | with 6 | low |
| 12 | discord: bridge thread messages as IRC reply chains | after 8 | medium |
| 13 | docs: add FORK.md ledger of carried patches | fork only | none |

### 1. irc: guard the bridge nick against concurrent access
- **Why:** girc runs handlers concurrently. `skipPrivMsg` rewrote `b.Nick` on every message while other handlers read it. The race detector flags it (M5).
- **Change:** `getNick` and `setNick` behind a mutex, at every use.
- **Files:** `bridge/irc/irc.go`, `bridge/irc/handlers.go`.
- **Hot spots:** `skipPrivMsg`, `handleJoinPartPrefix`, `handleNewConnection`, `doConnect`.
- **Upstream:** comes from RustyBower's 42wim #2260, which became the first commit of #2261.

### 2. irc: make connection-state flags race-safe
- **Why:** girc handlers, `Send`, `doSend`, `doJoin` and the reconnect loop all share the connection flags, message lengths and RELAYMSG counters (M4).
- **Change:** those fields become `atomic.Bool` and `atomic.Int64`.
- **Files:** `bridge/irc/irc.go`, `bridge/irc/handlers.go`.
- **Hot spots:** the `Birc` struct (gofmt realigned it), `Connect`, `Disconnect`, `doConnect`, `Send`, `doSend`, `handleJoinPartPrefix`.
- **Note:** `Casemapping` and `RelayMsgSep` stay plain strings. Only `UseRelayMsg` writes them.

### 3. irc: stop a disconnected client's reconnect loop
- **Why:** a pre-existing crash. After a gateway-level reconnect, the old `doConnect` loop wakes 60s later and dials the new client. girc panics with "use of connect more than once".
- **Change:** `doConnect(client, quit)` loops on its own client and returns when `Disconnect()` closes `quit`. The connected goroutine stops too, so it no longer leaks.
- **Also:** `Disconnect()` marks the next connection as a first one. The new client then removes `handleOther` like the first one did.
- **Test infra:** this commit brings the fake IRCv3 server, the private-viper test config and the test bridge helpers. The 60s delay becomes `var reconnectDelay`.
- **Files:** `bridge/irc/irc.go`, `bridge/irc/bridge_test.go`, `bridge/irc/fakeserver_test.go`, `bridge/irc/reconnect_test.go`.
- **Hot spots:** `Connect`, `Disconnect`, `doConnect`.

### 4. irc: take our nick from RPL_WELCOME's target on reconnect
- **Why:** a pre-existing silent stall. When girc reconnects on its own, the RPL_WELCOME handler stored the server's name as our nick. The bridge then ignored its own JOIN and dropped every message to IRC.
- **Change:** use `event.Params[0]`, the nick RPL_WELCOME is addressed to.
- **Files:** `bridge/irc/irc.go`, `bridge/irc/reconnect_test.go`.
- **Hot spots:** the RPL_WELCOME closure in `doConnect`.

### 5. irc: only wait for NickServ when it is configured
- **Why:** `handleNickServ` slept 5s on every connection, even with no NickServ set. It also slowed every end-to-end test.
- **Change:** sleep only when `NickServNick` is set. IDENTIFY, Q and SASL setups keep the delay.
- **Files:** `bridge/irc/handlers.go`, `bridge/irc/nickserv_test.go`.
- **Hot spots:** `handleNickServ`.

### 6. irc: support PreserveThreading with IRCv3 reply tags
- **Why:** the core feature. IRC replies carry their parent to other bridges, and replies from other bridges get IRCv3 reply tags.
- **Change:** most code lives in `bridge/irc/threading.go`. The hot files only get small hooks.
- **Audit fixes inside:** H1, H2, H3, M1, M3 and M6. Section 3 explains each.
- **Files:** `bridge/irc/threading.go`, `bridge/irc/threading_test.go`, `bridge/irc/irc.go`, `bridge/irc/handlers.go`, `docs/protocols/irc/settings.md`, `matterbridge.toml.sample`.
- **Hot spots:** `Connect`, `Send`, `doSend`, `handlePrivMsg`, `handleOther`.
- **Upstream:** overlaps org's msgid and async send work (#142, #150, #159).

### 7. irc: accept the +reply tag on incoming replies
- **Why:** `+reply` is the ratified name, and clients such as IRCCloud send it.
- **Files:** `bridge/irc/threading.go`, `bridge/irc/threading_test.go`.

### 8. discord: only reply to snowflake parents, never fail on missing ones
- **Why:** when a parent never reached Discord, the gateway passes its ID on another bridge, such as `irc <msgid>`. Discord rejected the reference and dropped the message (M2).
- **Change:** reply only to snowflake IDs, and to the first part of a split parent (`123;456`). Set `fail_if_not_exists=false`, so a deleted parent doesn't drop the reply.
- **Files:** `bridge/discord/discord.go`, `bridge/discord/helpers.go`, tests.
- **Hot spots:** discord `Send`, `handleEventBotUser`.

### 9. discord: build reply quotes from the processed message text
- **Why:** a pre-existing bug. `handleQuote` used the raw content, so quotes reached IRC with `<@123>` mentions and `<:emote:456>` codes.
- **Change:** quote the processed text, and clean the quoted parent the same way.
- **Files:** `bridge/discord/handlers.go`, tests.
- **Hot spots:** `handleQuote`.
- **Upstream:** builds on org's inline reply quotes (#124).

### 10. discord: set the username apart in threaded replies sent by the bot
- **Why:** webhooks can't reply, so the bot sends replies in webhook channels. The webhook-style name ran into the text: `RustyCloudirc reply`.
- **Change:** `**RustyCloud** irc reply`, with markdown in the name escaped.
- **Files:** `bridge/discord/discord.go`, `bridge/discord/helpers.go`, tests.
- **Hot spots:** discord `Send`.
- **Upstream:** overlaps org #135.

### 11. irc: send the ratified +reply tag alongside +draft/reply
- **Why:** clients that know only one name still see threads.
- **Change:** send both tags, minus any that `CLIENTTAGDENY` denies.
- **Files:** `bridge/irc/threading.go`, `bridge/irc/threading_test.go`, docs, sample config.

### 12. discord: bridge thread messages as IRC reply chains
- **Why:** Discord thread messages were dropped, because a thread's channel ID matched no bridged channel.
- **Change:** relay thread messages from the parent channel as replies to the thread's root. Post IRC replies to threaded messages into the thread. Section 3 has the details.
- **Bug fix inside:** ThreadCreated (type 18) messages leaked the thread name to IRC as if the creator said it.
- **Files:** `bridge/discord/threads.go`, `bridge/discord/threads_test.go`, `bridge/discord/discord.go`, `bridge/discord/handlers.go`, `bridge/discord/webhook.go`, `bridge/discord/transmitter/transmitter.go`, `docs/protocols/discord/README.md`.
- **Hot spots:** discord `Send`, `messageCreate`, `messageDelete`, `Connect`, the transmitter's `Send` and `Edit`.

### Related upstream work

| Reference | What it is | Relevant to |
|---|---|---|
| 42wim #2261 | RustyBower's reply threading PR, the design origin | 1, 6, 7, 10 |
| 42wim #2260 | RustyBower's PR "irc: fix data race on the bridge's nick", the first commit of #2261 | 1 |
| org #124 | inline Discord reply quotes, merged | 9 |
| org #135 | overlaps the webhook reply username | 10 |
| org #142, #159 | msgid and async send for IRC | 6, 7, 11 |
| org #150 | "Make matterbridge fully asynchronous", open issue | 6 |
| org #190 | IRC message splitting (`MessageSplit`), fixed | 6, H2 |

## 3. Design decisions and evidence

### A Discord thread is an IRC reply chain
- Kirk's rule: an inline Discord reply is not a thread.
- IRCv3 has no thread primitive. The ratified [client-tags/reply](https://ircv3.net/specs/client-tags/reply) spec (PR #535) leaves threads to clients, which render `+reply` chains as threads. The threading proposal (#226) was closed.
- So a Discord thread maps to an IRC reply chain on the thread's root. Plain IRC clients see no marker.
- The root is the message the thread started from. A standalone thread has none, so its first relayed message becomes the root and goes out untagged.
- IRC can't say "start a thread". The bridge only uses threads that already exist on Discord, and never creates one.
- All thread handling is gated on the Discord account's `PreserveThreading`. Without it, thread messages are dropped as before.
- Replies of any kind need `PreserveThreading=true` on both the IRC and the Discord account.

### Both reply tag names, CLIENTTAGDENY per tag
- The bridge sends `+reply` and `+draft/reply`, as IRCCloud does. It reads either.
- It checks `CLIENTTAGDENY` for each tag and leaves out denied ones.
- 129irc sends `CLIENTTAGDENY=*,-draft/typing,-typing,-draft/channel-context,-draft/reply,-reply`, so both pass there.

### Local IDs mapped to msgids through echo-message
- `Send` queues lines for a throttled sender, so it can't wait for the server's msgid.
- It returns a local ID (`mbirc-...`) at once. When the server echoes the first line, the local ID maps to the real msgid.
- `echo-message` and `labeled-response` are only requested with `PreserveThreading`.

### Labels over text matching (M3)
- With `labeled-response`, the first line of each message carries `label=<local ID>`. The echo comes back with the same label.
- Text matching breaks when the server rewrites the line, for example colour stripping on a `+S` channel with `ColorNicks`.
- girc 1.1.1 has no labeled-response code. It still requests any `SupportedCaps` entry the server offers, and keeps every received tag.
- Exception: with `MessageSplit=false`, girc splits long lines and copies tags to every part. Labels must not be reused, so those lines are text-matched.

### Tags in the raw line (H2)
- With `MessageSplit`, org splits lines itself and sends them with `SendRawNoSplit`. `girc.Client.Send` would split them again.
- So `tagLine` writes the tags into the raw line, for all six send paths, RELAYMSG included.
- girc parses raw lines and writes tag values back unescaped. `validMsgID` therefore refuses msgids that would need escaping.

### Never ask girc from Send (H3)
- girc holds its client lock while dialing, with a 500s timeout. A girc call from `Send` would block the gateway.
- Caps come from parsing CAP ACK, CAP DEL and the ISUPPORT `CLIENTTAGDENY` token into per-client atomics.

### One threading handler per client (H1)
- girc only hands echoes to `ALL_EVENTS` handlers.
- `Connect` registers the threading handler on every new client. Once connected, it removes `handleOther` by its handle and never clears `ALL_EVENTS`.

### Reply guards (M1, M2)
- IRC: a reply tag only names a msgid seen in that same channel. The gateway's fallback can hand over IDs such as `discord 1234`.
- Discord: a reference only names a snowflake, with `fail_if_not_exists=false`. A missing parent never drops the message.

### Echoes never logged (M6)
- With echo-message, the server echoes NickServ IDENTIFY back. `handleOther` skips every echo, in every debug level.
- Removing `handleOther` alone would not help. On the first connection it goes only after IDENTIFY and `RunCommands` are sent.
- `DebugLevel=2` dumps raw girc traffic, which includes the outgoing IDENTIFY. That predates the fork.

### Atomics and the nick guard (M4, M5)
- The end-to-end tests run against the fake server under `-race`. They flagged the nick and every shared connection flag.

### NickServ wait only when configured
- The 5s wait exists for NickServ to answer IDENTIFY. With no NickServ configured, nothing is sent, so the wait is skipped.

### Discord quirks found live
- A standalone thread gets a ThreadCreated (type 18) system message in its channel whose ID equals the thread ID. Seen live: message 1561520819278970880 in thread 1561520819278970880. The starter lookup ignores system messages.
- Types 18 (ThreadCreated) and 21 (ThreadStarterMessage) are filtered. Type 18 had leaked the thread name to IRC as if the creator said it.
- Webhooks can't reply. Replies in webhook channels go out from the bot as `**nick** text`.
- Webhooks can post into threads. The fork sends with `thread_id`, which keeps the IRC nick and avatar. Webhook edits need `?thread_id` too.
- The bot joins each new thread in a bridged channel once. Discord only delivers private thread messages to members.

### The two pre-existing reconnect bugs
- Gateway reconnect crash: see patch 3. Plain matterbridge-org exits about 60s after a ping-timeout reconnect.
- girc reconnect stall: see patch 4. Plain matterbridge-org can drop every message to IRC until a restart.

## 4. Known limitations

| Area | Limitation |
|---|---|
| `MessageSplit=false` | Lines carry no label, so echoes are matched by text. A server that rewrites lines, such as `+S` with `ColorNicks`, then breaks threading of replies to those messages. |
| RELAYMSG | Tracking assumes the server returns our label on the relayed copy. Nobody has checked a real RELAYMSG server. Without `MessageSplit`, RELAYMSG messages aren't tracked. 129irc doesn't use RELAYMSG. |
| Split Discord parents | A parent sent to Discord in several parts (`123;456`) is replied to, or threaded, through its first part. |
| Thread roots | When the starter was never relayed, or left the gateway cache, other bridges get the thread's messages unthreaded. |
| Standalone threads | Their first message reaches IRC as a top-level line, by design. |
| First message order | Handler goroutines decide which message counts as "first relayed", under a lock. Two near-simultaneous first messages may follow processing order. |
| API calls | One `GET /channels/{id}` per unknown channel and per uncached reply parent. Results are cached in LRUs, 4xx answers included. Transient errors aren't cached. |
| Forum channels | Not treated specially. A forum post's starter is inside its thread, so the root is usually that starter. |
| Bot sends into threads | Without webhooks, they carry no inline reference to the parent. Prod uses webhooks. |
| Unguarded state | `b.i`, `Casemapping` and `RelayMsgSep` stay unguarded, as upstream has them. |

## 5. Rebasing onto a new upstream

In the fork, `origin` is r3cursive/matterbridge and `upstream` is matterbridge-org.
Add `upstream` once in a new clone:

```bash
git remote add upstream https://github.com/matterbridge-org/matterbridge.git
```

Then:

```bash
git fetch upstream
git checkout master && git merge --ff-only upstream/master && git push origin master
git checkout irc-threading && git rebase master
```

Expect conflicts in `bridge/irc/irc.go`, `bridge/irc/handlers.go`, `bridge/discord/discord.go` and `bridge/discord/handlers.go`.
Each hook must keep doing what this list says.

| Hook | Must preserve |
|---|---|
| irc `Birc` struct | `nickMu`, the atomic flags, `quit` and `threads` fields |
| irc `Connect` | creates `b.quit`, calls `setupThreading(i)`, keeps the `handleOther` handle, starts `doConnect(i, b.quit)` |
| irc connected goroutine | selects on `b.connected` and `quit`, removes only `handleOther`, never clears `ALL_EVENTS` |
| irc `Disconnect` | closes `b.quit` first, sets `FirstConnection` back to true |
| irc `doConnect` | loops on its own client, returns on `quit`, sets the nick from `event.Params[0]` |
| irc `Send` | gets `newSendID()` before splitting, puts it on the first line only, returns it, never calls girc |
| irc `doSend` | passes every raw line through `tagLine`, keeps `SendRawNoSplit` for `MessageSplit` |
| irc `handlePrivMsg` | calls `threadIncoming` before sending to the gateway |
| irc `handleOther` | returns on `event.Echo` before anything else |
| irc `handleNickServ` | sleeps only when `NickServNick` is set |
| irc nick uses | go through `getNick` and `setNick` |
| discord `Send` | cuts the parent at `;`, drops non-snowflake parents, routes through `threadTarget` before the webhook check |
| discord `handleEventBotUser` | sets `FailIfNotExists` to false on references |
| discord `messageCreate` | returns on thread system messages, resolves threads, calls `handleThreadMessage` after the empty-message check |
| discord `messageDelete` | resolves thread channels through `getChannelOrThreadName` |
| discord `handleQuote` | quotes the processed text, cleans the quoted parent |
| discord `Connect` | registers `threadCreate` |
| discord webhook and transmitter | pass `threadID` to `Send` and `Edit` |

Then test and build:

```bash
go test -race -tags goolm ./bridge/irc/... ./bridge/discord/... ./gateway/...
go build -tags goolm -o matterbridge .
```

Then run the live suites from `r3cursive/dis4irc-deploy`:

```bash
python3 -I scripts/e2e/bridge_e2e.py --suite all --binary ./matterbridge
```

That needs `.env.e2e` with the test guild and two bot tokens, as `docs/MATTERBRIDGE_FORK.md` there describes.
When all suites pass, set `MATTERBRIDGE_VERSION` in dis4irc-deploy's `Dockerfile` to the new `irc-threading` SHA.

## 6. When upstream lands a patch

| If upstream | Then |
|---|---|
| merges #135 | drop patch 10 |
| implements IRC msgid or async send (#142, #150, #159) | compare with patches 6, 7 and 11, then drop or adapt them and their hooks |
| fixes the gateway reconnect crash | drop patch 3, but keep its test infra, which later patches use |
| fixes the RPL_WELCOME nick | drop patch 4 |
| guards the nick or the connection flags | drop patch 1 or 2 |
| skips the NickServ wait | drop patch 5 |
| fixes Discord reply references or quotes | drop patch 8 or 9 |

Update this file in the same rebase.
