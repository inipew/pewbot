package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	tele "gopkg.in/telebot.v4"

	"pewbot/internal/kit"
)

type Adapter struct {
	cfg Config
	log *slog.Logger

	bot *tele.Bot
	out chan<- kit.Update
}

func New(cfg Config, log *slog.Logger) (*Adapter, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("telegram token is empty")
	}
	timeout := time.Duration(cfg.PollTimeoutSec)
	if timeout <= 0 {
		timeout = 10
	}
	b, err := tele.NewBot(tele.Settings{
		Token:  cfg.Token,
		Poller: &tele.LongPoller{Timeout: timeout * time.Second},
	})
	if err != nil {
		return nil, err
	}
	return &Adapter{cfg: cfg, log: log, bot: b}, nil
}

func (a *Adapter) Start(ctx context.Context, out chan<- kit.Update) error {
	a.out = out

	a.bot.Handle(tele.OnText, func(c tele.Context) error {
		m := c.Message()
		if m == nil {
			return nil
		}
		up := kit.Update{
			Kind: kit.UpdateMessage,
			Message: &kit.Message{
				ID:           m.ID,
				ChatID:       m.Chat.ID,
				ThreadID:     threadIDFromMsg(m),
				FromID:       m.Sender.ID,
				FromUsername: m.Sender.Username,
				Text:         m.Text,
				IsGroup:      m.Chat.Type != tele.ChatPrivate,
			},
		}
		select {
		case out <- up:
		default:
			// drop to keep bot responsive
		}
		return nil
	})

	a.bot.Handle(tele.OnCallback, func(c tele.Context) error {
		cb := c.Callback()
		if cb == nil || cb.Message == nil {
			return nil
		}
		up := kit.Update{
			Kind: kit.UpdateCallback,
			Callback: &kit.Callback{
				ID:        cb.ID,
				FromID:    cb.Sender.ID,
				ChatID:    cb.Message.Chat.ID,
				ThreadID:  threadIDFromMsg(cb.Message),
				MessageID: cb.Message.ID,
				Data:      cb.Data,
			},
		}
		select {
		case out <- up:
		default:
		}
		return nil
	})

	go func() {
		a.log.Info("telegram polling started")
		a.bot.Start()
	}()
	return nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	if a.bot != nil {
		a.bot.Stop()
	}
	return nil
}

func (a *Adapter) SendText(ctx context.Context, to kit.ChatTarget, text string, opt *kit.SendOptions) (kit.MessageRef, error) {
	if opt == nil {
		opt = &kit.SendOptions{}
	}
	chat := &tele.Chat{ID: to.ChatID}
	sendOpt := &tele.SendOptions{
		ParseMode:             opt.ParseMode,
		DisableWebPagePreview: opt.DisablePreview,
		ThreadID:              to.ThreadID,
	}
	if opt.ReplyMarkupAdapter != nil {
		if rm, ok := opt.ReplyMarkupAdapter.(*tele.ReplyMarkup); ok {
			sendOpt.ReplyMarkup = rm
		}
	}

	msg, err := a.bot.Send(chat, text, sendOpt)
	if err != nil {
		return kit.MessageRef{}, err
	}
	return kit.MessageRef{ChatID: to.ChatID, ThreadID: to.ThreadID, MessageID: msg.ID}, nil
}

func (a *Adapter) EditText(ctx context.Context, ref kit.MessageRef, text string, opt *kit.SendOptions) error {
	if opt == nil {
		opt = &kit.SendOptions{}
	}
	m := &tele.Message{ID: ref.MessageID, Chat: &tele.Chat{ID: ref.ChatID}}
	sendOpt := &tele.SendOptions{ParseMode: opt.ParseMode, DisableWebPagePreview: opt.DisablePreview}
	if opt.ReplyMarkupAdapter != nil {
		if rm, ok := opt.ReplyMarkupAdapter.(*tele.ReplyMarkup); ok {
			sendOpt.ReplyMarkup = rm
		}
	}
	_, err := a.bot.Edit(m, text, sendOpt)
	return err
}

func (a *Adapter) AnswerCallback(ctx context.Context, callbackID string, text string) error {
	return a.bot.Respond(&tele.Callback{ID: callbackID}, &tele.CallbackResponse{Text: text})
}

func threadIDFromMsg(m *tele.Message) int {
	return m.ThreadID
}
