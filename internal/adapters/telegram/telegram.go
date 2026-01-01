package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	tele "gopkg.in/telebot.v4"

	"pewbot/internal/kit"
)

type Adapter struct {
	cfg Config
	log *slog.Logger

	bot       *tele.Bot
	out       chan<- kit.Update
	runCancel context.CancelFunc
	runWG     sync.WaitGroup
	runMu     sync.Mutex
	running   bool

	menuMu   sync.Mutex
	menuHash uint64
	http     *http.Client
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
	return &Adapter{cfg: cfg, log: log, bot: b, http: &http.Client{Timeout: 8 * time.Second}}, nil
}

func (a *Adapter) Start(ctx context.Context, out chan<- kit.Update) error {
	a.runMu.Lock()
	if a.running {
		a.runMu.Unlock()
		return nil
	}
	a.running = true
	a.out = out
	rctx, cancel := context.WithCancel(ctx)
	a.runCancel = cancel
	a.runWG.Add(1)
	a.runMu.Unlock()

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
			},
		}
		select {
		case out <- up:
		default:
		}
		return nil
	})

	a.bot.Handle(tele.OnCallback, func(c tele.Context) error {
		cb := c.Callback()
		m := c.Message()
		if cb == nil || m == nil {
			return nil
		}
		up := kit.Update{
			Kind: kit.UpdateCallback,
			Callback: &kit.Callback{
				ChatID:   m.Chat.ID,
				ThreadID: threadIDFromMsg(m),
				FromID:   cb.Sender.ID,
				Data:     cb.Data,
			},
		}
		select {
		case out <- up:
		default:
		}
		return nil
	})

	go func() {
		defer a.runWG.Done()
		// Ensure we stop telebot when context is cancelled.
		go func() {
			<-rctx.Done()
			a.bot.Stop()
		}()
		a.log.Info("polling started")
		a.bot.Start() // blocks until Stop() called
	}()

	return nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	// Best-effort graceful stop. Never block shutdown for too long on Telegram long-poll.
	a.runMu.Lock()
	cancel := a.runCancel
	a.runCancel = nil
	wasRunning := a.running
	a.running = false
	a.runMu.Unlock()

	a.log.Info("stopping")
	if !wasRunning {
		a.log.Debug("telegram stop called but not running")
		return nil
	}

	if cancel != nil {
		cancel()
	}

	// telebot Stop is expected to be fast; run it async just in case.
	if a.bot != nil {
		go a.bot.Stop()
	}

	done := make(chan struct{})
	go func() {
		a.runWG.Wait()
		close(done)
	}()

	// Grace window: keep shutdown snappy even if getUpdates long-poll is still waiting.
	grace := 2 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		rem := time.Until(dl)
		if rem > 0 && rem < grace {
			grace = rem
		}
	}
	t := time.NewTimer(grace)
	defer t.Stop()

	select {
	case <-done:
		a.log.Info("polling stopped")
		return nil
	case <-ctx.Done():
		a.log.Warn("telegram stop cancelled", slog.String("err", ctx.Err().Error()))
		return ctx.Err()
	case <-t.C:
		a.log.Warn("telegram stop grace elapsed; continuing shutdown")
		return nil
	}
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

// UpdateMenuCommands updates Telegram's global /menu command list (setMyCommands).
// Best-effort: it only performs a network call when the command list changes.
func (a *Adapter) UpdateMenuCommands(ctx context.Context, cmds []kit.BotCommand) error {
	a.menuMu.Lock()
	defer a.menuMu.Unlock()

	h := fnv.New64a()
	for _, c := range cmds {
		h.Write([]byte(c.Command))
		h.Write([]byte{0})
		h.Write([]byte(c.Description))
		h.Write([]byte{0})
	}
	sum := h.Sum64()
	if sum == a.menuHash {
		return nil
	}

	type cmd struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	payload := struct {
		Commands []cmd `json:"commands"`
	}{Commands: make([]cmd, 0, len(cmds))}

	for _, c := range cmds {
		if c.Command == "" {
			continue
		}
		d := c.Description
		if d == "" {
			d = c.Command
		}
		if len(d) > 256 {
			d = d[:256]
		}
		payload.Commands = append(payload.Commands, cmd{Command: c.Command, Description: d})
		if len(payload.Commands) >= 100 {
			break
		}
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := "https://api.telegram.org/bot" + strings.TrimSpace(a.cfg.Token) + "/setMyCommands"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := a.http
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var out struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	if resp.StatusCode/100 != 2 || !out.OK {
		if out.Description != "" {
			return fmt.Errorf("telegram setMyCommands failed: %s (code=%d http=%d)", out.Description, out.ErrorCode, resp.StatusCode)
		}
		return fmt.Errorf("telegram setMyCommands failed: http=%d", resp.StatusCode)
	}

	a.menuHash = sum
	a.log.Info("menu commands updated", slog.Int("count", len(payload.Commands)))
	return nil
}
