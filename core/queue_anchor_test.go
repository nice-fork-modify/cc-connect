package core

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// anchorUpdate records one in-place edit performed by anchorPlatform.
type anchorUpdate struct {
	Handle  any
	Content string
}

// anchorPlatform is a platform that can open editable messages
// (PreviewStarter) and edit them afterwards (MessageUpdater), which is what
// the queue-ack anchor needs. Plain Send/Reply calls still land in the
// embedded stub's sent list, so a test can tell "edited the anchor" apart from
// "posted another message".
type anchorPlatform struct {
	stubPlatformEngine
	amu      sync.Mutex
	opens    []string
	updates  []anchorUpdate
	next     int
	startErr error
}

func (p *anchorPlatform) SendPreviewStart(_ context.Context, _ any, content string) (any, error) {
	p.amu.Lock()
	defer p.amu.Unlock()
	if p.startErr != nil {
		return nil, p.startErr
	}
	p.next++
	handle := fmt.Sprintf("anchor-%d", p.next)
	p.opens = append(p.opens, content)
	return handle, nil
}

func (p *anchorPlatform) UpdateMessage(_ context.Context, handle any, content string) error {
	p.amu.Lock()
	defer p.amu.Unlock()
	p.updates = append(p.updates, anchorUpdate{Handle: handle, Content: content})
	return nil
}

func (p *anchorPlatform) getOpens() []string {
	p.amu.Lock()
	defer p.amu.Unlock()
	out := make([]string, len(p.opens))
	copy(out, p.opens)
	return out
}

func (p *anchorPlatform) getUpdates() []anchorUpdate {
	p.amu.Lock()
	defer p.amu.Unlock()
	out := make([]anchorUpdate, len(p.updates))
	copy(out, p.updates)
	return out
}

// runTwoTurns drives one in-flight turn plus the queued turn behind it: the
// first EventResult ends turn #1, the event loop then sends the queued prompt,
// and the second EventResult ends turn #2.
func runTwoTurns(t *testing.T, e *Engine, state *interactiveState, session *Session, key string, sess *queuingAgentSession, first, second string) {
	t.Helper()

	go func() {
		sess.events <- Event{Type: EventResult, Content: first, Done: true}
		sess.sendMu.Lock()
		for len(sess.sendCalls) == 0 {
			sess.sendMu.Unlock()
			time.Sleep(5 * time.Millisecond)
			sess.sendMu.Lock()
		}
		sess.sendMu.Unlock()
		sess.events <- Event{Type: EventResult, Content: second, Done: true}
	}()

	session.AddHistory("user", "initial-msg")

	sendDone := make(chan error, 1)
	sendDone <- nil

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "msg1", time.Now(), nil, sendDone, "ctx-turn1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("processInteractiveEvents did not complete in time")
	}
}

// TestQueueAnchor_AckBecomesFinalAnswer is the core P2 contract: a message that
// waits in the queue produces exactly ONE bot message, which is first the
// "queued" ack and is then edited in place into that turn's final answer.
func TestQueueAnchor_AckBecomesFinalAnswer(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newQueuingSession("qs-anchor")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:anchor-user"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx-turn1",
		turnSeq:        1,
		currentTurnSeq: 1,
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	if !e.queueMessageForBusySession(p, &Message{SessionKey: key, Content: "queued-msg", ReplyCtx: "ctx-turn2"}, key) {
		t.Fatal("expected the message to be queued")
	}

	wantAck := "[#2 📬] " + e.i18n.T(MsgMessageQueued)
	if opens := p.getOpens(); len(opens) != 1 || opens[0] != wantAck {
		t.Fatalf("anchor opens = %v, want [%q]", opens, wantAck)
	}
	if sent := p.getSent(); len(sent) != 0 {
		t.Fatalf("ack should be the anchor, not a plain message; plain sends = %v", sent)
	}

	state.mu.Lock()
	ackHandle := state.pendingMessages[0].ackHandle
	ackText := state.pendingMessages[0].ackText
	state.mu.Unlock()
	if ackHandle == nil {
		t.Fatal("queued message kept no ack handle")
	}
	if ackText != wantAck {
		t.Fatalf("stored ack text = %q, want %q", ackText, wantAck)
	}

	runTwoTurns(t, e, state, session, key, sess, "response1", "response2")

	// Turn #1 never had an anchor, so its answer is a plain message. Turn #2's
	// answer must NOT be one: it replaced the ack.
	if sent := p.getSent(); len(sent) != 1 || sent[0] != "[#1 ✅] response1" {
		t.Fatalf("plain sends = %v, want only turn #1's answer", sent)
	}
	if opens := p.getOpens(); len(opens) != 1 {
		t.Fatalf("anchor opens = %v, want 1 (the ack, reused by turn #2)", opens)
	}
	// Two edits of the same message: "queued" → "working" when the turn starts,
	// then → the final answer.
	updates := p.getUpdates()
	want := []anchorUpdate{
		{Handle: ackHandle, Content: "[#2 ⏳] " + e.i18n.T(MsgStarting)},
		{Handle: ackHandle, Content: "[#2 ✅] response2"},
	}
	if len(updates) != len(want) {
		t.Fatalf("anchor edits = %v, want %v", updates, want)
	}
	for i := range want {
		if updates[i] != want[i] {
			t.Fatalf("anchor edit[%d] = %v, want %v", i, updates[i], want[i])
		}
	}
}

// TestQueueAnchor_AdoptedAckFlipsToWorking covers the default configuration
// (instant reply off, which is the shipped default): when the queued turn starts
// running, its ack must stop saying "queued" right away — by editing that same
// message, without adding a new one.
func TestQueueAnchor_AdoptedAckFlipsToWorking(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newQueuingSession("qs-anchor-working")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	if e.instantReply.Enabled {
		t.Fatal("instant reply must be off by default for this test to mean anything")
	}

	key := "test:anchor-working-user"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx-turn1",
		turnSeq:        2,
		currentTurnSeq: 1,
		pendingMessages: []queuedMessage{{
			platform:  p,
			replyCtx:  "ctx-turn2",
			content:   "queued-msg",
			turnSeq:   2,
			ackHandle: "ack-1",
			ackText:   "[#2 📬] " + e.i18n.T(MsgMessageQueued),
		}},
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	runTwoTurns(t, e, state, session, key, sess, "response1", "response2")

	// Turn #2 adds no message of its own: no new anchor, and the only plain
	// message is turn #1's answer (turn #1 had nothing to adopt).
	if opens := p.getOpens(); len(opens) != 0 {
		t.Fatalf("anchor opens = %v, want none (instant reply is off)", opens)
	}
	if sent := p.getSent(); len(sent) != 1 || sent[0] != "[#1 ✅] response1" {
		t.Fatalf("plain sends = %v, want only turn #1's answer", sent)
	}

	updates := p.getUpdates()
	want := []anchorUpdate{
		{Handle: "ack-1", Content: "[#2 ⏳] " + e.i18n.T(MsgStarting)},
		{Handle: "ack-1", Content: "[#2 ✅] response2"},
	}
	if len(updates) != len(want) {
		t.Fatalf("anchor edits = %v, want %v", updates, want)
	}
	for i := range want {
		if updates[i] != want[i] {
			t.Fatalf("anchor edit[%d] = %v, want %v", i, updates[i], want[i])
		}
	}
}

// TestQueueAnchor_FallsBackWithoutPreviewStarter verifies the degraded path:
// a platform that cannot open an editable message keeps the pre-P2 behavior —
// the ack is a plain reply and no handle is retained.
func TestQueueAnchor_FallsBackWithoutPreviewStarter(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newQueuingSession("qs-noanchor")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:noanchor-user"
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx-turn1",
		turnSeq:        1,
		currentTurnSeq: 1,
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	if !e.queueMessageForBusySession(p, &Message{SessionKey: key, Content: "queued-msg", ReplyCtx: "ctx-turn2"}, key) {
		t.Fatal("expected the message to be queued")
	}

	wantAck := "[#2 📬] " + e.i18n.T(MsgMessageQueued)
	if sent := p.getSent(); len(sent) != 1 || sent[0] != wantAck {
		t.Fatalf("plain sends = %v, want [%q]", sent, wantAck)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.pendingMessages[0].ackHandle != nil {
		t.Fatalf("ackHandle = %v, want nil on a platform without PreviewStarter", state.pendingMessages[0].ackHandle)
	}
}

// TestQueueAnchor_FallsBackWhenAnchorSendFails covers the other degraded case:
// the platform advertises PreviewStarter but the send fails.
func TestQueueAnchor_FallsBackWhenAnchorSendFails(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, startErr: fmt.Errorf("boom")}
	sess := newQueuingSession("qs-anchor-fail")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:anchor-fail-user"
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx-turn1",
		turnSeq:        1,
		currentTurnSeq: 1,
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	if !e.queueMessageForBusySession(p, &Message{SessionKey: key, Content: "queued-msg", ReplyCtx: "ctx-turn2"}, key) {
		t.Fatal("expected the message to be queued")
	}

	wantAck := "[#2 📬] " + e.i18n.T(MsgMessageQueued)
	if sent := p.getSent(); len(sent) != 1 || sent[0] != wantAck {
		t.Fatalf("plain sends = %v, want the ack to fall back to a plain message: [%q]", sent, wantAck)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.pendingMessages[0].ackHandle != nil {
		t.Fatalf("ackHandle = %v, want nil after a failed anchor send", state.pendingMessages[0].ackHandle)
	}
}

// TestQueueAnchor_InstantReplyReusesAnchor verifies requirement 3: the
// "processing" notice edits the queued turn's anchor instead of adding a
// second message.
func TestQueueAnchor_InstantReplyReusesAnchor(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newQueuingSession("qs-anchor-instant")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetInstantReply(InstantReplyCfg{Enabled: true, Content: "🤔 Thinking..."})

	key := "test:anchor-instant-user"
	session := e.sessions.GetOrCreateActive(key)
	ackText := "[#2 📬] " + e.i18n.T(MsgMessageQueued)
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx-turn1",
		turnSeq:        2,
		currentTurnSeq: 1,
		pendingMessages: []queuedMessage{{
			platform:  p,
			replyCtx:  "ctx-turn2",
			content:   "queued-msg",
			turnSeq:   2,
			ackHandle: "ack-1",
			ackText:   ackText,
		}},
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	runTwoTurns(t, e, state, session, key, sess, "response1", "response2")

	// Neither turn falls back to a plain message.
	if sent := p.getSent(); len(sent) != 0 {
		t.Fatalf("plain sends = %v, want none (both turns own an anchor)", sent)
	}
	// Turn #1 had no ack to adopt, so its notice opens the turn's anchor.
	// Turn #2 must NOT open one: it adopted the queue ack.
	opens := p.getOpens()
	if len(opens) != 1 || opens[0] != "[#1 ⏳] 🤔 Thinking..." {
		t.Fatalf("anchor opens = %v, want only turn #1's notice", opens)
	}

	updates := p.getUpdates()
	want := []anchorUpdate{
		{Handle: "anchor-1", Content: "[#1 ✅] response1"},
		{Handle: "ack-1", Content: "[#2 ⏳] 🤔 Thinking..."},
		{Handle: "ack-1", Content: "[#2 ✅] response2"},
	}
	if len(updates) != len(want) {
		t.Fatalf("anchor edits = %v, want %v", updates, want)
	}
	for i := range want {
		if updates[i] != want[i] {
			t.Fatalf("anchor edit[%d] = %v, want %v", i, updates[i], want[i])
		}
	}
}

// TestQueueAnchor_StaleDropFinalizesAck verifies a queued message dropped for
// being superseded does not leave its ack sitting at "queued".
func TestQueueAnchor_StaleDropFinalizesAck(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newQueuingSession("qs-anchor-stale")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:anchor-stale-user"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{
		agentSession:                 sess,
		platform:                     p,
		replyCtx:                     "ctx-turn1",
		turnSeq:                      2,
		currentTurnSeq:               1,
		currentTurnUserMessageTimeMs: 2000,
		pendingMessages: []queuedMessage{{
			platform:          p,
			replyCtx:          "ctx-turn2",
			content:           "stale-msg",
			turnSeq:           2,
			userMessageTimeMs: 1000,
			ackHandle:         "ack-1",
			ackText:           "[#2 📬] " + e.i18n.T(MsgMessageQueued),
		}},
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	sendDone := make(chan error, 1)
	sendDone <- nil
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "msg1", time.Now(), nil, sendDone, "ctx-turn1")
		close(done)
	}()
	sess.events <- Event{Type: EventResult, Content: "response1", Done: true}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("processInteractiveEvents did not complete in time")
	}

	state.mu.Lock()
	remaining := len(state.pendingMessages)
	state.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("pendingMessages = %d, want the stale message dropped", remaining)
	}

	updates := p.getUpdates()
	want := "[#2 ⏹] " + e.i18n.T(MsgQueuedSuperseded)
	if len(updates) != 1 || updates[0].Handle != "ack-1" || updates[0].Content != want {
		t.Fatalf("anchor edits = %v, want one edit of ack-1 to %q", updates, want)
	}
}

// TestQueueAnchor_StopFinalizesAck verifies /stop closes out the acks of the
// queued messages it discards.
func TestQueueAnchor_StopFinalizesAck(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newControllableSession("agent-anchor-stop")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:anchor-stop-user"
	e.interactiveMu.Lock()
	e.interactiveStates[key] = &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx",
		turnSeq:        2,
		currentTurnSeq: 1,
		pendingMessages: []queuedMessage{{
			platform:  p,
			replyCtx:  "ctx-turn2",
			content:   "queued-msg",
			turnSeq:   2,
			ackHandle: "ack-1",
			ackText:   "[#2 📬] " + e.i18n.T(MsgMessageQueued),
		}},
	}
	e.interactiveMu.Unlock()

	e.cmdStop(p, &Message{SessionKey: key, ReplyCtx: "ctx"})

	updates := p.getUpdates()
	want := "[#2 ⏹] " + e.i18n.T(MsgQueuedCancelled)
	if len(updates) != 1 || updates[0].Handle != "ack-1" || updates[0].Content != want {
		t.Fatalf("anchor edits = %v, want one edit of ack-1 to %q", updates, want)
	}
}

// TestQueueAnchor_RecallFinalizesAck verifies that recalling a queued user
// message closes out its ack too.
func TestQueueAnchor_RecallFinalizesAck(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	key := "test:anchor-recall-user"
	e.interactiveMu.Lock()
	e.interactiveStates[key] = &interactiveState{
		platform: p,
		replyCtx: "ctx",
		turnSeq:  2,
		pendingMessages: []queuedMessage{{
			messageID: "m-2",
			platform:  p,
			replyCtx:  "ctx-turn2",
			content:   "queued-msg",
			turnSeq:   2,
			ackHandle: "ack-1",
			ackText:   "[#2 📬] " + e.i18n.T(MsgMessageQueued),
		}},
	}
	e.interactiveMu.Unlock()

	if _, ok := e.removeQueuedMessageByID("m-2"); !ok {
		t.Fatal("expected the queued message to be removed")
	}

	updates := p.getUpdates()
	want := "[#2 ⏹] " + e.i18n.T(MsgQueuedCancelled)
	if len(updates) != 1 || updates[0].Handle != "ack-1" || updates[0].Content != want {
		t.Fatalf("anchor edits = %v, want one edit of ack-1 to %q", updates, want)
	}
}

// TestStreamPreview_AdoptHandleFinishNotSkipped guards the lastSentViaUpdate
// rule of adoptHandle: the adopted message was delivered by SendPreviewStart,
// so finish() must still issue one UpdateMessage — even when the final text is
// byte-identical to what the message already shows. Flipping that flag to true
// would silently swallow the final answer.
func TestStreamPreview_AdoptHandleFinishNotSkipped(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sp := newStreamPreview(DefaultStreamPreviewCfg(), p, "ctx", context.Background(), nil)

	sp.adoptHandle("ack-1", "[#2 📬] queued")
	if sp.previewMsgID != "ack-1" {
		t.Fatalf("previewMsgID = %v, want ack-1", sp.previewMsgID)
	}
	if sp.lastSentViaUpdate {
		t.Fatal("lastSentViaUpdate must stay false after adoptHandle")
	}
	if !sp.lastSentAt.IsZero() {
		t.Fatal("lastSentAt must be zeroed so the first frame is not throttled")
	}

	if !sp.finish("[#2 📬] queued", "") {
		t.Fatal("finish should report that the adopted message carried the final text")
	}
	updates := p.getUpdates()
	if len(updates) != 1 || updates[0].Handle != "ack-1" || updates[0].Content != "[#2 📬] queued" {
		t.Fatalf("anchor edits = %v, want one edit of ack-1 even for identical text", updates)
	}
}

// TestStreamPreview_AdoptHandleIgnoresNil keeps the no-anchor case a no-op.
func TestStreamPreview_AdoptHandleIgnoresNil(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sp := newStreamPreview(DefaultStreamPreviewCfg(), p, "ctx", context.Background(), nil)

	sp.adoptHandle(nil, "ignored")
	if sp.previewMsgID != nil {
		t.Fatalf("previewMsgID = %v, want nil", sp.previewMsgID)
	}
	if sp.lastSentText != "" {
		t.Fatalf("lastSentText = %q, want empty", sp.lastSentText)
	}
}
