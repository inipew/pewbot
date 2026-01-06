package transport

import "context"

type UpdateKind string

const (
	UpdateMessage  UpdateKind = "message"
	UpdateCallback UpdateKind = "callback"
)

type Update struct {
	Kind     UpdateKind
	Message  *Message
	Callback *Callback
}

type Message struct {
	ID           int
	ChatID       int64
	ThreadID     int // telegram forum topic thread id (0 if none)
	FromID       int64
	FromUsername string
	Text         string
	IsGroup      bool
}

type Callback struct {
	ID        string
	FromID    int64
	ChatID    int64
	ThreadID  int
	MessageID int
	Data      string
}

type ChatTarget struct {
	ChatID   int64
	ThreadID int
}

type MessageRef struct {
	ChatID    int64
	ThreadID  int
	MessageID int
}

type SendOptions struct {
	ParseMode          string
	DisablePreview     bool
	ReplyMarkupAdapter any // adapter-specific markup (Telegram: *telebot.ReplyMarkup)
}

type Notification struct {
	Channel  string // "telegram" now
	Priority int    // 0 low.. 10 high
	Target   ChatTarget
	Text     string
	Options  *SendOptions
}

type Adapter interface {
	Start(ctx context.Context, out chan<- Update) error
	Stop(ctx context.Context) error

	SendText(ctx context.Context, to ChatTarget, text string, opt *SendOptions) (MessageRef, error)
	EditText(ctx context.Context, ref MessageRef, text string, opt *SendOptions) error
	AnswerCallback(ctx context.Context, callbackID string, text string) error
}

// BotCommand represents a single bot command menu entry.
type BotCommand struct {
	Command     string
	Description string
}

// CommandMenuUpdater is an optional interface that adapters can implement
// to update platform-specific bot command menus (e.g. Telegram /menu list).
type CommandMenuUpdater interface {
	UpdateMenuCommands(ctx context.Context, cmds []BotCommand) error
}
