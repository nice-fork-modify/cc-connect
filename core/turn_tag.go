package core

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Turn status icons used by the turn marker prefix (see Engine.turnTag).
const (
	turnIconWorking  = "⏳" // the turn is being processed
	turnIconQueued   = "📬" // the message was accepted into the FIFO queue
	turnIconDone     = "✅" // the turn finished normally
	turnIconAwaiting = "❓" // waiting for the user (permission / AskUserQuestion)
	turnIconFailed   = "❌" // the turn ended with an error
	turnIconStopped  = "⏹" // the turn was aborted by /stop
	turnIconImage    = "🖼" // a side-channel image belonging to the turn
	turnIconFile     = "📎" // a side-channel file belonging to the turn
)

// turnTag renders a turn marker like "[#3 ⏳] " for correlating bot output
// with the user message that triggered it. Returns "" when disabled or when
// seq is not assigned.
func (e *Engine) turnTag(seq int, icon string) string {
	if !e.display.TurnTag || seq <= 0 {
		return ""
	}
	return fmt.Sprintf("[#%d %s] ", seq, icon)
}

// currentTurnTag renders the turn marker for the state's in-flight turn.
// The caller must NOT hold state.mu.
func (e *Engine) currentTurnTag(state *interactiveState, icon string) string {
	if !e.display.TurnTag || state == nil {
		return ""
	}
	state.mu.Lock()
	seq := state.currentTurnSeq
	state.mu.Unlock()
	return e.turnTag(seq, icon)
}

// startingNotice composes the "processing" notice that opens a turn's anchor
// message. A custom text is used verbatim. The default i18n text already leads
// with the working icon, which the turn marker also carries, so that duplicate
// is dropped whenever a marker is present.
func (e *Engine) startingNotice(tag, custom string) string {
	if custom != "" {
		return tag + custom
	}
	text := e.i18n.T(MsgStarting)
	if tag != "" {
		text = strings.TrimPrefix(text, turnIconWorking+" ")
	}
	return tag + text
}

// attachmentCaption renders the caption stamped onto a side-channel attachment
// (an image or file the agent pushes mid-turn via `cc-connect send`) so it can
// be traced back to the user message that triggered the turn, the same way text
// output is. Returns "" when no turn is in flight — a proactive or scheduled
// send belongs to no turn and must not borrow another turn's number.
func (e *Engine) attachmentCaption(state *interactiveState, icon string) string {
	return strings.TrimRight(e.currentTurnTag(state, icon), " ")
}

// turnTagHolder carries the turn marker that the stream preview injects into
// every outgoing frame. The icon flips from "working" to "done" at the end of
// the turn while the preview's own flush timer may still fire, so the value is
// stored atomically.
type turnTagHolder struct{ v atomic.Value }

func (h *turnTagHolder) set(tag string) { h.v.Store(tag) }

func (h *turnTagHolder) get() string {
	tag, _ := h.v.Load().(string)
	return tag
}

// prefixRenderer wraps an outgoing-content renderer so every frame it produces
// carries the current turn marker. Empty content stays empty: the stream
// preview treats "" as "nothing to deliver" and a bare marker must not be
// pushed to the platform.
func (h *turnTagHolder) prefixRenderer(render func(string) string) func(string) string {
	return func(content string) string {
		if content == "" {
			return ""
		}
		return h.get() + render(content)
	}
}
