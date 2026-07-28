package solar

import "time"

// Account represents a Solar Network account.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Nick string `json:"nick"`
}

// ChatRoom represents a Solar Network chat room.
type ChatRoom struct {
	ID            string       `json:"id"`
	Type          int          `json:"type"`
	DirectMembers []ChatMember `json:"members"`
}

// ChatMember is a member of a chat room.
type ChatMember struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	Account   Account   `json:"account"`
	Nick      string    `json:"nick"`
	ChatRoom  *ChatRoom `json:"chat_room,omitempty"`
}

// ChatMessage is a message in a Solar Network chat room.
type ChatMessage struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Content    string    `json:"content"`
	Sender     ChatMember `json:"sender"`
	ChatRoomID string    `json:"chat_room_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// Packet is a Solar Network websocket packet.
type Packet struct {
	Type         string `json:"type"`
	Data         any    `json:"data"`
	Endpoint     string `json:"endpoint,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}
