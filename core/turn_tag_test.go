package core

import (
	"fmt"
	"testing"
	"time"
)

// TestTurnTag_Render covers the marker renderer itself: enabled by default,
// empty for an unassigned sequence, and empty when the feature is turned off.
func TestTurnTag_Render(t *testing.T) {
	e := newTestEngine()

	if got := e.turnTag(3, turnIconWorking); got != "[#3 ⏳] " {
		t.Fatalf("turnTag(3, working) = %q, want %q", got, "[#3 ⏳] ")
	}
	if got := e.turnTag(1, turnIconDone); got != "[#1 ✅] " {
		t.Fatalf("turnTag(1, done) = %q, want %q", got, "[#1 ✅] ")
	}
	if got := e.turnTag(0, turnIconWorking); got != "" {
		t.Fatalf("turnTag(0, working) = %q, want empty (no sequence assigned)", got)
	}

	e.display.TurnTag = false
	if got := e.turnTag(3, turnIconWorking); got != "" {
		t.Fatalf("turnTag with turn_tag disabled = %q, want empty", got)
	}
}

// TestTurnTag_QueuedMessagesNumberedInAcceptOrder verifies that the turn number
// is allocated when a message is accepted into the queue (not when its turn
// starts), so the queue ack can show the number right away and the numbering
// matches the order the user sent the messages in.
func TestTurnTag_QueuedMessagesNumberedInAcceptOrder(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newQueuingSession("qs-seq")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:seq-user"
	// Turn #1 is already in flight.
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx-1",
		turnSeq:        1,
		currentTurnSeq: 1,
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	for i, content := range []string{"msg2", "msg3"} {
		if ok := e.queueMessageForBusySession(p, &Message{
			SessionKey: key,
			Content:    content,
			ReplyCtx:   fmt.Sprintf("ctx-%d", i+2),
		}, key); !ok {
			t.Fatalf("expected %s to be queued", content)
		}
	}

	state.mu.Lock()
	queued := append([]queuedMessage(nil), state.pendingMessages...)
	state.mu.Unlock()

	if len(queued) != 2 {
		t.Fatalf("queue depth = %d, want 2", len(queued))
	}
	if queued[0].turnSeq != 2 || queued[1].turnSeq != 3 {
		t.Fatalf("queued turn numbers = [%d %d], want [2 3]", queued[0].turnSeq, queued[1].turnSeq)
	}

	// The ack sent for each queued message carries its own number.
	sent := p.getSent()
	wantAck := []string{
		"[#2 📬] " + e.i18n.T(MsgMessageQueued),
		"[#3 📬] " + e.i18n.T(MsgMessageQueued),
	}
	if len(sent) != len(wantAck) {
		t.Fatalf("acks = %v, want %v", sent, wantAck)
	}
	for i := range wantAck {
		if sent[i] != wantAck[i] {
			t.Fatalf("ack[%d] = %q, want %q", i, sent[i], wantAck[i])
		}
	}
}

// TestTurnTag_RejectedMessageGetsNoNumber verifies that a message dropped
// because the queue is full does not consume a turn number: it was never
// accepted, so no output will ever carry its marker.
func TestTurnTag_RejectedMessageGetsNoNumber(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newQueuingSession("qs-seq-full")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:seq-full-user"
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx",
		turnSeq:        1,
		currentTurnSeq: 1,
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	for i := 0; i < defaultMaxQueuedMessages; i++ {
		msg := &Message{SessionKey: key, Content: fmt.Sprintf("msg-%d", i), ReplyCtx: fmt.Sprintf("ctx-%d", i)}
		if ok := e.queueMessageForBusySession(p, msg, key); !ok {
			t.Fatalf("expected msg-%d to be queued", i)
		}
	}

	state.mu.Lock()
	seqBefore := state.turnSeq
	state.mu.Unlock()

	e.queueMessageForBusySession(p, &Message{SessionKey: key, Content: "overflow", ReplyCtx: "ctx-of"}, key)

	state.mu.Lock()
	seqAfter := state.turnSeq
	state.mu.Unlock()
	if seqAfter != seqBefore {
		t.Fatalf("turnSeq after rejected message = %d, want %d unchanged", seqAfter, seqBefore)
	}
}

// TestTurnTag_IconFollowsTurnState verifies the marker travels with the turn:
// the instant reply is "working", the final answer is "done", and both carry
// the same number.
func TestTurnTag_IconFollowsTurnState(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	agent := &resultAgent{session: newResultAgentSession("agent reply")}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetInstantReply(InstantReplyCfg{Enabled: true, Content: "🤔 Thinking..."})

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		UserID:     "u1",
		UserName:   "user",
		Content:    "hello",
		ReplyCtx:   "ctx",
	})

	sent := waitForPlatformSend(p, 2, 2*time.Second)
	if len(sent) < 2 {
		t.Fatalf("sent = %v, want instant reply + final answer", sent)
	}
	if sent[0] != "[#1 ⏳] 🤔 Thinking..." {
		t.Fatalf("instant reply = %q, want working marker", sent[0])
	}
	if got := sent[len(sent)-1]; got != "[#1 ✅] agent reply" {
		t.Fatalf("final answer = %q, want done marker", got)
	}
}

// TestTurnTag_DisabledLeavesOutputUnchanged is the explicit turn_tag = false
// case: no marker anywhere.
func TestTurnTag_DisabledLeavesOutputUnchanged(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	agent := &resultAgent{session: newResultAgentSession("agent reply")}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.display.TurnTag = false
	e.SetInstantReply(InstantReplyCfg{Enabled: true, Content: "🤔 Thinking..."})

	e.handleMessage(p, &Message{
		SessionKey: "test:user1",
		Platform:   "test",
		UserID:     "u1",
		UserName:   "user",
		Content:    "hello",
		ReplyCtx:   "ctx",
	})

	sent := waitForPlatformSend(p, 2, 2*time.Second)
	if len(sent) < 2 {
		t.Fatalf("sent = %v, want instant reply + final answer", sent)
	}
	if sent[0] != "🤔 Thinking..." {
		t.Fatalf("instant reply = %q, want no marker", sent[0])
	}
	if got := sent[len(sent)-1]; got != "agent reply" {
		t.Fatalf("final answer = %q, want no marker", got)
	}
}

// TestTurnTag_FailedTurnUsesFailedIcon verifies EventError output carries ❌.
func TestTurnTag_FailedTurnUsesFailedIcon(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newQueuingSession("qs-err")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:err-user"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx-err",
		turnSeq:        2,
		currentTurnSeq: 2,
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	sendDone := make(chan error, 1)
	sendDone <- nil

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "msg", time.Now(), nil, sendDone, "ctx-err")
		close(done)
	}()

	sess.events <- Event{Type: EventError, Error: fmt.Errorf("boom")}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("processInteractiveEvents did not return after EventError")
	}

	sent := p.getSent()
	want := "[#2 ❌] " + fmt.Sprintf(e.i18n.T(MsgError), "boom")
	if len(sent) != 1 || sent[0] != want {
		t.Fatalf("sent = %v, want [%q]", sent, want)
	}
}

// TestTurnTag_StopUsesStoppedIcon verifies the /stop notice carries ⏹ with the
// aborted turn's number.
func TestTurnTag_StopUsesStoppedIcon(t *testing.T) {
	sess := newControllableSession("agent-stop")
	agent := &controllableAgent{nextSession: sess}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:stop-user"
	e.interactiveMu.Lock()
	e.interactiveStates[key] = &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx",
		turnSeq:        4,
		currentTurnSeq: 4,
	}
	e.interactiveMu.Unlock()

	e.cmdStop(p, &Message{SessionKey: key, ReplyCtx: "ctx"})

	sent := p.getSent()
	want := "[#4 ⏹] " + e.i18n.T(MsgExecutionStopped)
	if len(sent) != 1 || sent[0] != want {
		t.Fatalf("sent = %v, want [%q]", sent, want)
	}
}

// TestTurnTag_NonUserTurnInheritsNoNumber verifies that the in-flight turn
// number is cleared when the event loop exits, so a turn that never went
// through message acceptance (cron / timer, which reach processInteractiveEvents
// directly and may reuse the user's live session) cannot be labelled with the
// previous user turn's number.
func TestTurnTag_NonUserTurnInheritsNoNumber(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newQueuingSession("qs-nonuser")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:nonuser"
	session := e.sessions.GetOrCreateActive(key)
	// A user turn (#3) is in flight on this state.
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx-user",
		turnSeq:        3,
		currentTurnSeq: 3,
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	runTurn := func(msgID, replyCtx, result string) {
		sendDone := make(chan error, 1)
		sendDone <- nil
		done := make(chan struct{})
		go func() {
			e.processInteractiveEvents(state, session, e.sessions, key, msgID, time.Now(), nil, sendDone, replyCtx)
			close(done)
		}()
		sess.events <- Event{Type: EventResult, Content: result, Done: true}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("processInteractiveEvents did not complete for %s", msgID)
		}
	}

	runTurn("msg-user", "ctx-user", "user answer")

	state.mu.Lock()
	seqAfter := state.currentTurnSeq
	state.mu.Unlock()
	if seqAfter != 0 {
		t.Fatalf("currentTurnSeq after the user turn = %d, want 0", seqAfter)
	}

	// A cron-style turn reusing the same state: no acceptance, no number.
	runTurn("msg-cron", "ctx-cron", "cron output")

	sent := p.getSent()
	want := []string{"[#3 ✅] user answer", "cron output"}
	if len(sent) != len(want) {
		t.Fatalf("sent = %v, want %v", sent, want)
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("sent[%d] = %q, want %q", i, sent[i], want[i])
		}
	}
}

// TestTurnTag_QueuedTurnNumbersAndDoneReaction verifies two things for
// back-to-back messages handled by one event loop:
//   - each turn's final answer carries that turn's own number;
//   - the finished turn gets its "done" reaction before the next queued turn
//     starts, using the finished turn's reply context (previously only the last
//     message in a queued chain ever got one).
func TestTurnTag_QueuedTurnNumbersAndDoneReaction(t *testing.T) {
	p := &stubDoneReactionPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newQueuingSession("qs-done")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:done-user"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx-turn1",
		turnSeq:        2,
		currentTurnSeq: 1,
		pendingMessages: []queuedMessage{
			{platform: p, replyCtx: "ctx-turn2", content: "queued-msg", turnSeq: 2},
		},
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	go func() {
		sess.events <- Event{Type: EventResult, Content: "response1", Done: true}
		sess.sendMu.Lock()
		for len(sess.sendCalls) == 0 {
			sess.sendMu.Unlock()
			time.Sleep(5 * time.Millisecond)
			sess.sendMu.Lock()
		}
		sess.sendMu.Unlock()
		sess.events <- Event{Type: EventResult, Content: "response2", Done: true}
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

	sent := p.getSent()
	want := []string{"[#1 ✅] response1", "[#2 ✅] response2"}
	if len(sent) != len(want) {
		t.Fatalf("sent = %v, want %v", sent, want)
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("sent[%d] = %q, want %q", i, sent[i], want[i])
		}
	}

	count, ctxs := p.doneSnapshot()
	if count != 2 {
		t.Fatalf("done reactions = %d, want 2 (one per finished turn); ctxs=%v", count, ctxs)
	}
	if ctxs[0] != "ctx-turn1" || ctxs[1] != "ctx-turn2" {
		t.Fatalf("done reaction contexts = %v, want [ctx-turn1 ctx-turn2]", ctxs)
	}
}

// TestTurnTag_MidTurnSegmentsCarryMarker is the regression for mid-turn text
// segments losing the turn marker.
//
// In compact mode the first tool call freezes and detaches the stream preview,
// which leaves it degraded for the rest of the turn (unfreeze is only called
// after a permission prompt). Every later segment is therefore delivered as a
// plain message instead of a preview frame. Those plain messages must still
// carry the marker: a turn that interleaves text and tool calls — the "explain,
// generate an image, report" shape — otherwise emits unlabelled output that
// cannot be traced back to the user message that triggered it.
func TestTurnTag_MidTurnSegmentsCarryMarker(t *testing.T) {
	p := &anchorPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newQueuingSession("qs-midturn")
	agent := &controllableAgent{nextSession: sess}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	// compact: tool progress is hidden, so each tool call is purely a segment
	// boundary and the only plain messages are the text segments themselves.
	e.display.Mode = "compact"
	e.display.ToolMessages = false
	e.display.ThinkingMessages = false

	key := "test:midturn-user"
	session := e.sessions.GetOrCreateActive(key)
	state := &interactiveState{
		agentSession:   sess,
		platform:       p,
		replyCtx:       "ctx",
		turnSeq:        4,
		currentTurnSeq: 4,
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	go func() {
		// Segment one streams into the preview; the tool call then freezes and
		// detaches it. Segment two has nowhere to stream to and is flushed as a
		// plain message at the next tool boundary — this is the one that used to
		// lose its marker. Segment three is the final answer.
		sess.events <- Event{Type: EventText, Content: "segment one"}
		sess.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "generate"}
		sess.events <- Event{Type: EventText, Content: "segment two"}
		sess.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "send-image"}
		sess.events <- Event{Type: EventText, Content: "segment three"}
		sess.events <- Event{Type: EventResult, Content: "segment three", Done: true}
	}()

	sendDone := make(chan error, 1)
	sendDone <- nil

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "msg", time.Now(), nil, sendDone, "ctx")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("processInteractiveEvents did not complete in time")
	}

	sent := p.getSent()
	if len(sent) != 2 {
		t.Fatalf("plain sends = %v, want segment two + the final answer", sent)
	}
	if sent[0] != "[#4 ⏳] segment two" {
		t.Fatalf("mid-turn segment = %q, want %q", sent[0], "[#4 ⏳] segment two")
	}
	if sent[1] != "[#4 ✅] segment three" {
		t.Fatalf("final answer = %q, want %q", sent[1], "[#4 ✅] segment three")
	}
}

// TestTurnTag_AttachmentsCarryMarker covers side-channel attachments: an image
// or file the agent pushes mid-turn via `cc-connect send` is a separate platform
// message, so without a marker it is the one piece of a turn's output that
// cannot be traced back to the triggering user message.
func TestTurnTag_AttachmentsCarryMarker(t *testing.T) {
	p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.interactiveStates["session-1"] = &interactiveState{
		platform:       p,
		replyCtx:       "ctx-1",
		turnSeq:        4,
		currentTurnSeq: 4, // turn #4 is in flight
	}

	err := e.SendToSessionWithAttachments(
		"session-1",
		"",
		[]ImageAttachment{{MimeType: "image/png", Data: []byte("img"), FileName: "chart.png"}},
		[]FileAttachment{{MimeType: "text/plain", Data: []byte("doc"), FileName: "report.txt"}},
		nil, false)
	if err != nil {
		t.Fatalf("SendToSessionWithAttachments returned error: %v", err)
	}

	if len(p.images) != 1 || p.images[0].Caption != "[#4 🖼]" {
		t.Fatalf("image caption = %#v, want %q", p.images, "[#4 🖼]")
	}
	if len(p.files) != 1 || p.files[0].Caption != "[#4 📎]" {
		t.Fatalf("file caption = %#v, want %q", p.files, "[#4 📎]")
	}
}

// TestTurnTag_ProactiveAttachmentGetsNoMarker is the other half of the contract:
// a scheduled or proactive send belongs to no turn (currentTurnSeq is zeroed
// when a turn ends), so it must not borrow the previous turn's number.
func TestTurnTag_ProactiveAttachmentGetsNoMarker(t *testing.T) {
	p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.interactiveStates["session-1"] = &interactiveState{
		platform:       p,
		replyCtx:       "ctx-1",
		turnSeq:        4, // four turns have run
		currentTurnSeq: 0, // ...but none is in flight right now
	}

	err := e.SendToSessionWithAttachments(
		"session-1",
		"",
		[]ImageAttachment{{MimeType: "image/png", Data: []byte("img"), FileName: "chart.png"}},
		nil, nil, false)
	if err != nil {
		t.Fatalf("SendToSessionWithAttachments returned error: %v", err)
	}
	if len(p.images) != 1 || p.images[0].Caption != "" {
		t.Fatalf("image caption = %#v, want empty (no turn in flight)", p.images)
	}
}
