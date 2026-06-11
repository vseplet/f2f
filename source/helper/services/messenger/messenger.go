// Package messenger is the messaging service: direct messages and named
// channels carried over the QUIC bus. State lives in memory for now (the
// Service); the SQLite Store in store.go is the persistence backend, kept
// type-compatible so it can be wired in later without reshaping anything.
//
// Model — there is no chat server, so everything a recipient needs travels
// in the message:
//
//   - A named channel's ID is "<owner_pub>/<name>", identical on every node
//     with no coordinator. A DM is the degenerate channel: no name, keyed
//     locally by the other peer's pub.
//   - EVERY channel message carries a snapshot of the full member roster, so
//     membership is self-healing — a node that missed earlier traffic still
//     converges, and a fresh member learns the whole roster from the one
//     message that added it. Channel lifecycle (create / add / remove) is
//     itself just a message with a Type; the channel's state is the fold of
//     its messages. Only the owner's messages may change the roster.
package messenger

// Message is one event in a conversation: a text line, or a channel
// lifecycle event. The owning camp is implicit (the database file / the
// running camp), so it isn't stored on the row. Author display names are
// NOT stored — resolve them from From at render time.
type Message struct {
	ID   string `json:"id"`   // globally unique: "<from8>-<seq>-<ts>"
	Kind string `json:"kind"` // "dm" | "channel"
	Peer string `json:"peer"` // conversation key: dm→peer pub; channel→channel id
	Type string `json:"type"` // "text" | "create" | "add" | "remove"
	From string `json:"from"` // author pub (bus-attested on receive)
	To   string `json:"to"`   // recipient pub for a DM; "" for a channel post
	Body string `json:"body"` // text payload (Type=="text")
	// Members is the full channel roster snapshot carried by every channel
	// message. Authoritative only when From is the channel owner.
	Members []string `json:"members,omitempty"`
	// Targets names the peers a membership event acted on (added or removed),
	// so the UI can say WHO — the full roster alone doesn't reveal the delta.
	Targets []string `json:"targets,omitempty"`
	// File is an optional inline attachment (a small photo, clip or document)
	// riding alongside the text. nil for a plain message.
	File *Attachment `json:"file,omitempty"`
	TS   int64       `json:"ts"` // unix ms
	Mine bool        `json:"mine"`
}

// MaxAttachment caps an inline attachment's raw size. The bus frame limit is
// 16 MiB and base64 inflates by ~33%, so this keeps a message well under it;
// bigger files belong in the file-sharing drop, not chat.
const MaxAttachment = 8 << 20 // 8 MiB

// Attachment is an inline file carried by a message. Data holds the raw bytes
// (encoding/json marshals a []byte as a base64 string, so it travels and
// persists as text without extra work).
type Attachment struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Size int    `json:"size"`
	Data []byte `json:"data"`
}

// Channel is a named room. ID is "<owner_pub>/<name>"; Owner and Name are
// also derivable from it (see SplitChannelID) but kept explicit for clarity.
type Channel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Owner     string   `json:"owner"`
	Members   []string `json:"members"`
	CreatedAt int64    `json:"created_at"` // unix ms
}
