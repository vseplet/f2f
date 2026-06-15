// Package message is messaging over the block engine: a message is a block in
// the per-channel scope "message:<channelBid>". Posting creates a block;
// editing writes a new version of it (so edit history is free); deleting
// tombstones it. Delivery, offline catch-up and convergence come from db sync —
// there's no separate outbox. Membership/identity of the channel live in its
// channel block (see package channels).
package message

import (
	"encoding/json"
	"sort"

	"github.com/vseplet/f2f/source/helper/db/blocks"
	"github.com/vseplet/f2f/source/helper/db/blocks/attach"
)

// ScopePrefix namespaces a channel's message log.
const ScopePrefix = "message:"

// blockType marks message blocks (Type = "block.message").
const blockType = "message"

// MaxAttachment caps an inline attachment's raw size. Alias of attach.Max —
// kept for existing callers.
const MaxAttachment = attach.Max

// Scope returns the message scope for a channel bid.
func Scope(channelBID string) string { return ScopePrefix + channelBID }

// Attachment is the shared attachment primitive (see package attach). A message
// carries one as a field on its block; notes carry it as a standalone block.file.
type Attachment = attach.Attachment

// content is the message block's payload.
type content struct {
	Body    string      `json:"body,omitempty"`
	File    *Attachment `json:"file,omitempty"`
	ReplyTo string      `json:"reply_to,omitempty"` // message bid this replies to
	Thread  string      `json:"thread,omitempty"`   // thread-root message bid
}

// Message is the folded current state of one message block.
type Message struct {
	ID      string      `json:"id"`      // block bid
	Channel string      `json:"channel"` // channel bid
	From    string      `json:"from"`    // author pub (first version)
	Body    string      `json:"body"`
	File    *Attachment `json:"file,omitempty"`
	ReplyTo string      `json:"reply_to,omitempty"`
	Thread  string      `json:"thread,omitempty"`
	TS      int64       `json:"ts"`     // original post time (first version)
	Edited  bool        `json:"edited"` // more than one version
}

// Manager is the chat engine over the block engine.
type Manager struct{ blocks *blocks.Manager }

func New(b *blocks.Manager) *Manager { return &Manager{blocks: b} }

// Post adds a message to a channel and returns its id (block bid).
func (m *Manager) Post(s blocks.Signer, channelBID, body string, file *Attachment, replyTo, thread string) (string, error) {
	c, err := json.Marshal(content{Body: body, File: file, ReplyTo: replyTo, Thread: thread})
	if err != nil {
		return "", err
	}
	return m.blocks.Create(s, Scope(channelBID), blockType, c, "", "")
}

// Edit replaces a message's text/file with a new version (same id).
func (m *Manager) Edit(s blocks.Signer, channelBID, msgBID, body string, file *Attachment) error {
	// Preserve reply/thread links from the current head.
	cur := m.Get(channelBID, msgBID)
	var reply, thread string
	if cur != nil {
		reply, thread = cur.ReplyTo, cur.Thread
	}
	c, err := json.Marshal(content{Body: body, File: file, ReplyTo: reply, Thread: thread})
	if err != nil {
		return err
	}
	return m.blocks.Update(s, Scope(channelBID), msgBID, c, nil)
}

// Delete tombstones a message.
func (m *Manager) Delete(s blocks.Signer, channelBID, msgBID string) error {
	return m.blocks.Delete(s, Scope(channelBID), msgBID, nil)
}

// Messages folds a channel's live messages, oldest first. Order is by the
// create version's Lamport (logical clock, monotonic) then id — wall-clock TS
// ties when two posts land in the same millisecond, so it can't be the key.
// Edits don't reorder (they keep the original create version's order).
func (m *Manager) Messages(channelBID string) []Message {
	type ordered struct {
		msg     Message
		lamport uint64
	}
	var tmp []ordered
	for _, b := range m.blocks.Blocks(Scope(channelBID)) {
		if b.Deleted {
			continue
		}
		var lp uint64
		if len(b.History) > 0 {
			lp = b.History[0].Lamport
		}
		tmp = append(tmp, ordered{toMessage(channelBID, b), lp})
	}
	sort.Slice(tmp, func(i, j int) bool {
		if tmp[i].lamport != tmp[j].lamport {
			return tmp[i].lamport < tmp[j].lamport
		}
		return tmp[i].msg.ID < tmp[j].msg.ID
	})
	out := make([]Message, len(tmp))
	for i := range tmp {
		out[i] = tmp[i].msg
	}
	return out
}

// Get folds a single message (nil if unknown/deleted).
func (m *Manager) Get(channelBID, msgBID string) *Message {
	b := m.blocks.Block(Scope(channelBID), msgBID)
	if b == nil || b.Deleted {
		return nil
	}
	msg := toMessage(channelBID, b)
	return &msg
}

func toMessage(channelBID string, b *blocks.Block) Message {
	var c content
	if n := len(b.Heads); n > 0 {
		_ = json.Unmarshal(b.Heads[n-1].Content, &c)
	}
	from, ts := "", int64(0)
	if len(b.History) > 0 {
		from, ts = b.History[0].Author, b.History[0].TS
	}
	return Message{
		ID: b.BID, Channel: channelBID, From: from,
		Body: c.Body, File: c.File, ReplyTo: c.ReplyTo, Thread: c.Thread,
		TS: ts, Edited: len(b.History) > 1,
	}
}
