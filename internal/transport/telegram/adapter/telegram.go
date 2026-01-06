package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	logx "pewbot/pkg/logx"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tele "gopkg.in/telebot.v4"

	rtsup "pewbot/internal/runtime/supervisor"

	kit "pewbot/internal/transport"
)

type Adapter struct {
	cfg Config
	log logx.Logger

	bot     *tele.Bot
	out     atomic.Value // stores (chan<- kit.Update)
	runMu   sync.Mutex
	running bool

	// sup owns adapter internal goroutines (poll loop, drop logger, stop watcher).
	// It is created on Start() and cancelled on Stop().
	sup *rtsup.Supervisor

	// droppedUpdates counts updates dropped because the consumer was slower than the Telegram poll loop.
	// This is logged periodically to avoid per-update log spam.
	droppedUpdates uint64

	menuMu   sync.Mutex
	menuHash uint64
	http     *http.Client
}

// Supervisor returns the adapter's internal supervisor (nil if not started).
// This is used for operational visibility (e.g. /health).
func (a *Adapter) Supervisor() *rtsup.Supervisor {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.sup
}

func (a *Adapter) registerHandlers() {
	// Handlers forward to the CURRENT output channel. Start() may swap it.
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
		a.sendUpdate(up)
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
				ID:        cb.ID,
				ChatID:    m.Chat.ID,
				ThreadID:  threadIDFromMsg(m),
				FromID:    cb.Sender.ID,
				MessageID: m.ID,
				Data:      cb.Data,
			},
		}
		a.sendUpdate(up)
		return nil
	})
}

func New(cfg Config, log logx.Logger) (*Adapter, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("telegram token is empty")
	}
	timeout := cfg.PollTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	b, err := tele.NewBot(tele.Settings{
		Token:  cfg.Token,
		Poller: &tele.LongPoller{Timeout: timeout},
	})
	if err != nil {
		return nil, err
	}
	if log.IsZero() {
		log = logx.Nop()
	}
	a := &Adapter{cfg: cfg, log: log, bot: b, http: &http.Client{Timeout: 8 * time.Second}}
	// Ensure atomic.Value is initialized with a stable dynamic type.
	var nilOut chan<- kit.Update
	a.out.Store(nilOut)
	a.registerHandlers()
	return a, nil
}

func (a *Adapter) sendUpdate(up kit.Update) {
	v := a.out.Load()
	out, _ := v.(chan<- kit.Update)
	if out == nil {
		return
	}
	select {
	case out <- up:
	default:
		atomic.AddUint64(&a.droppedUpdates, 1)
	}
}

func (a *Adapter) Start(ctx context.Context, out chan<- kit.Update) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.runMu.Lock()
	if a.running {
		a.runMu.Unlock()
		return nil
	}
	a.running = true
	a.out.Store(out)
	a.sup = rtsup.NewSupervisor(ctx,
		rtsup.WithLogger(a.log.With(logx.String("comp", "telegram.adapter"))),
		// adapter errors should not take down the whole app; treat as best-effort.
		rtsup.WithCancelOnError(false),
	)
	sup := a.sup
	a.runMu.Unlock()

	// Periodic summary for dropped updates (avoid noisy per-update logs).
	sup.Go0("updates.drop_report", func(c context.Context) {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-c.Done():
				// Final flush.
				if n := atomic.SwapUint64(&a.droppedUpdates, 0); n > 0 {
					a.log.Warn("incoming updates dropped (channel full)", logx.Uint64("count", n), logx.Int("chan_cap", cap(out)))
				}
				return
			case <-ticker.C:
				if n := atomic.SwapUint64(&a.droppedUpdates, 0); n > 0 {
					a.log.Warn("incoming updates dropped (channel full)", logx.Uint64("count", n), logx.Int("chan_cap", cap(out)))
				}
			}
		}
	})

	// Ensure we stop telebot when the adapter context is cancelled.
	sup.Go0("telebot.stop_on_cancel", func(c context.Context) {
		<-c.Done()
		if a.bot != nil {
			a.bot.Stop()
		}
	})

	// Telebot's Start() is a long-running loop. In some failure modes it can
	// exit unexpectedly; run it under a restart loop so the adapter self-heals.
	sup.GoRestart0("telebot.poll", func(c context.Context) {
		a.log.Info("polling started")
		// Start blocks until Stop() called.
		if a.bot != nil {
			a.bot.Start()
		}
		a.log.Info("polling stopped")
	},
		rtsup.WithRestartBackoff(500*time.Millisecond, 10*time.Second),
		rtsup.WithPublishFirstError(true),
		// Restart if Start() returns while context is still active.
		rtsup.WithStopOnCleanExit(false),
	)

	return nil
}

func (a *Adapter) Stop(ctx context.Context) error {
	// Best-effort graceful stop. Never block shutdown for too long on Telegram long-poll.
	a.runMu.Lock()
	sup := a.sup
	a.sup = nil
	wasRunning := a.running
	a.running = false
	var nilOut chan<- kit.Update
	a.out.Store(nilOut)
	a.runMu.Unlock()

	a.log.Info("stopping", logx.Uint64("dropped_updates_pending", atomic.LoadUint64(&a.droppedUpdates)))
	if !wasRunning {
		a.log.Debug("telegram stop called but not running")
		return nil
	}

	if sup != nil {
		sup.Cancel()
	}

	// telebot Stop is expected to be fast; run it async just in case.
	if a.bot != nil {
		go a.bot.Stop()
	}

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

	if sup == nil {
		return nil
	}

	wctx := ctx
	var cancel context.CancelFunc
	if grace > 0 {
		wctx, cancel = context.WithTimeout(ctx, grace)
		defer cancel()
	}

	if err := sup.Wait(wctx); err != nil {
		// Don't hard-fail shutdown for adapter; just report.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			a.log.Warn("telegram stop timed out", logx.Err(err))
			return nil
		}

		// If we're stopping (supervisor already canceled), avoid noisy warnings.
		if sup.Context().Err() != nil {
			a.log.Debug("telegram stopped with supervisor error", logx.Err(err))
			return nil
		}
		a.log.Warn("telegram stop error", logx.Err(err))
	}
	return nil
}

const telegramTextLimit = 4000

// splitTelegramText splits long messages into chunks that are safe to send to Telegram.
// It prefers newline boundaries and (best-effort) avoids splitting inside HTML tags when ParseMode is HTML.
func splitTelegramText(s string, limit int, parseMode string) []string {
	if limit <= 0 {
		limit = telegramTextLimit
	}
	rs := []rune(s)
	if len(rs) <= limit {
		return []string{s}
	}

	out := make([]string, 0, (len(rs)+limit-1)/limit)
	start := 0
	for start < len(rs) {
		end := start + limit
		if end > len(rs) {
			end = len(rs)
		}

		// Prefer splitting on a newline near the end of the window.
		if end < len(rs) {
			cut := -1
			for i := end - 1; i > start; i-- {
				if rs[i] == '\n' {
					// Avoid extremely small chunks.
					if i-start >= limit/3 {
						cut = i + 1
						break
					}
				}
			}
			if cut != -1 {
				end = cut
			}
		}

		// Best-effort: don't split inside a tag for HTML parse mode.
		if strings.EqualFold(parseMode, "HTML") && end < len(rs) {
			lastOpen := -1
			lastClose := -1
			for i := start; i < end; i++ {
				if rs[i] == '<' {
					lastOpen = i
				} else if rs[i] == '>' {
					lastClose = i
				}
			}
			if lastOpen > lastClose && lastOpen > start+1 {
				// Move end to the start of the dangling tag.
				end = lastOpen
				if end <= start {
					end = start + limit
					if end > len(rs) {
						end = len(rs)
					}
				}
			}
		}

		chunk := string(rs[start:end])
		chunk = strings.TrimRight(chunk, "\n")
		out = append(out, chunk)

		start = end
		// Skip leading newlines to avoid empty chunks.
		for start < len(rs) && rs[start] == '\n' {
			start++
		}
	}
	return out
}

func (a *Adapter) SendText(ctx context.Context, to kit.ChatTarget, text string, opt *kit.SendOptions) (kit.MessageRef, error) {
	if opt == nil {
		opt = &kit.SendOptions{}
	}

	chunks := splitTelegramText(text, telegramTextLimit, opt.ParseMode)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	chat := &tele.Chat{ID: to.ChatID}

	var first kit.MessageRef
	for i, chunk := range chunks {
		if ctx != nil {
			select {
			case <-ctx.Done():
				if first.ChatID != 0 {
					return first, ctx.Err()
				}
				return kit.MessageRef{}, ctx.Err()
			default:
			}
		}

		sendOpt := &tele.SendOptions{
			ParseMode:             opt.ParseMode,
			DisableWebPagePreview: opt.DisablePreview,
			ThreadID:              to.ThreadID,
		}

		// Attach markup only to the first message.
		if i == 0 && opt.ReplyMarkupAdapter != nil {
			if rm, ok := opt.ReplyMarkupAdapter.(*tele.ReplyMarkup); ok {
				sendOpt.ReplyMarkup = rm
			}
		}

		msg, err := a.bot.Send(chat, chunk, sendOpt)
		if err != nil {
			if first.ChatID != 0 {
				return first, err
			}
			return kit.MessageRef{}, err
		}

		if i == 0 {
			first = kit.MessageRef{ChatID: to.ChatID, ThreadID: to.ThreadID, MessageID: msg.ID}
		}
	}

	return first, nil
}

func (a *Adapter) EditText(ctx context.Context, ref kit.MessageRef, text string, opt *kit.SendOptions) error {
	if opt == nil {
		opt = &kit.SendOptions{}
	}

	chunks := splitTelegramText(text, telegramTextLimit, opt.ParseMode)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	m := &tele.Message{ID: ref.MessageID, Chat: &tele.Chat{ID: ref.ChatID}}
	sendOpt := &tele.SendOptions{ParseMode: opt.ParseMode, DisableWebPagePreview: opt.DisablePreview}
	if opt.ReplyMarkupAdapter != nil {
		if rm, ok := opt.ReplyMarkupAdapter.(*tele.ReplyMarkup); ok {
			sendOpt.ReplyMarkup = rm
		}
	}

	_, err := a.bot.Edit(m, chunks[0], sendOpt)
	if err != nil {
		return err
	}

	// If the text is too long to fit in a single edited message, send the remaining parts as new messages.
	if len(chunks) > 1 {
		to := kit.ChatTarget{ChatID: ref.ChatID, ThreadID: ref.ThreadID}
		chat := &tele.Chat{ID: to.ChatID}
		for _, chunk := range chunks[1:] {
			if ctx != nil {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
			}
			sendOpt2 := &tele.SendOptions{
				ParseMode:             opt.ParseMode,
				DisableWebPagePreview: opt.DisablePreview,
				ThreadID:              to.ThreadID,
			}
			if _, e := a.bot.Send(chat, chunk, sendOpt2); e != nil {
				return e
			}
		}
	}

	return nil
}

func (a *Adapter) AnswerCallback(ctx context.Context, callbackID string, text string) error {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
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
	a.log.Info("menu commands updated", logx.Int("count", len(payload.Commands)))
	return nil
}
