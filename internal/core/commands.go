package core

import (
	"context"
	"log/slog"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"pewbot/internal/kit"
)

type Access int

const (
	AccessEveryone Access = iota
	AccessOwnerOnly
)

type Command struct {
	// Route is a space-separated command path, e.g.:
	//   "ping"
	//   "systemd restart"
	Route       string
	Aliases     []string // root-level aliases, e.g. ["systemd_restart", "sr"]
	Description string
	Usage       string
	Access      Access

	PluginName string
	Timeout    time.Duration // optional per-command override
	Handle     HandlerFunc
}

type CallbackHandlerFunc func(ctx context.Context, req *Request, payload string) error

type CallbackRoute struct {
	Plugin      string
	Action      string
	Description string
	Timeout     time.Duration
	Handle      CallbackHandlerFunc
}

type Request struct {
	Update  kit.Update
	Chat    kit.ChatTarget
	FromID  int64
	Path    []string // matched command path tokens (for message updates)
	Command string   // convenience (route or callback key)
	Args    []string
	Payload string // callback payload (raw string)

	// Parsed arguments
	RawArgs   []string
	Flags     map[string]string
	BoolFlags map[string]bool
	ReqID     string

	Adapter     kit.Adapter
	Config      *Config
	Logger      *slog.Logger
	Services    *Services
	OwnerUserID []int64
}

type Services struct {
	Scheduler   SchedulerPort
	Broadcaster BroadcasterPort
	Notifier    NotifierPort
}

type SchedulerPort interface {
	Enabled() bool
	Snapshot() Snapshot

	AddCron(name, spec string, timeout time.Duration, job func(ctx context.Context) error) (string, error)
	AddCronOpt(name, spec string, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error)

	AddInterval(name string, every time.Duration, timeout time.Duration, job func(ctx context.Context) error) (string, error)
	AddIntervalOpt(name string, every time.Duration, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error)

	AddOnce(name string, at time.Time, timeout time.Duration, job func(ctx context.Context) error) (string, error)
	AddDaily(name, atHHMM string, timeout time.Duration, job func(ctx context.Context) error) (string, error)
	AddWeekly(name string, weekday time.Weekday, atHHMM string, timeout time.Duration, job func(ctx context.Context) error) (string, error)

	Remove(name string) bool
}

type BroadcasterPort interface {
	NewJob(name string, targets []kit.ChatTarget, text string, opt *kit.SendOptions) string
	StartJob(ctx context.Context, jobID string) error
	Status(jobID string) (any, bool)
}

type NotifierPort interface {
	Notify(ctx context.Context, n kit.Notification) error
}

type CommandManager struct {
	mu sync.RWMutex

	root  *cmdNode
	alias map[string]*cmdNode // alias -> leaf node

	cbMu      sync.RWMutex
	callbacks map[string]map[string]CallbackRoute // plugin -> action -> route

	owners []int64

	log     *slog.Logger
	adapter kit.Adapter
	cfgm    *ConfigManager
	serv    *Services

	jobs chan func()
}

func NewCommandManager(log *slog.Logger, adapter kit.Adapter, cfgm *ConfigManager, serv *Services, owners []int64) *CommandManager {
	// copy to avoid callers mutating the slice after construction
	ownCopy := append([]int64(nil), owners...)
	return &CommandManager{
		root:      newRoot(),
		alias:     map[string]*cmdNode{},
		callbacks: map[string]map[string]CallbackRoute{},
		log:       log,
		adapter:   adapter,
		cfgm:      cfgm,
		serv:      serv,
		owners:    ownCopy,
		jobs:      make(chan func(), 256),
	}
}

// SetOwners updates the owner list used for AccessOwnerOnly checks.
// Safe to call during hot-reload.
func (m *CommandManager) SetOwners(owners []int64) {
	// copy to avoid external mutation
	ownCopy := append([]int64(nil), owners...)
	m.mu.Lock()
	m.owners = ownCopy
	m.mu.Unlock()
}

func (m *CommandManager) ownersSnapshot() []int64 {
	m.mu.RLock()
	cp := append([]int64(nil), m.owners...)
	m.mu.RUnlock()
	return cp
}

func (m *CommandManager) SetRegistry(cmds []Command, cbs []CallbackRoute) {
	// always inject help
	helper := Command{
		Route:       "help",
		Aliases:     []string{"h"},
		Description: "show help",
		Usage:       "/help [cmd] [sub...]",
		Access:      AccessEveryone,
		Handle: func(ctx context.Context, req *Request) error {
			text := m.helpText(req.Args)
			_, _ = req.Adapter.SendText(ctx, req.Chat, text, &kit.SendOptions{DisablePreview: true})
			return nil
		},
	}
	cmds = append(cmds, helper)

	root := newRoot()
	alias := map[string]*cmdNode{}

	for _, c := range cmds {
		route := splitRoute(c.Route)
		if len(route) == 0 || c.Handle == nil {
			continue
		}
		cc := c // copy
		root.add(route, cc)

		leaf := root.find(route)
		// auto alias for multi-token routes: "a b" -> "a_b" (useful for Telegram menu shortcuts)
		if len(route) > 1 {
			auto := strings.Join(route, "_")
			if _, exists := alias[auto]; !exists {
				alias[auto] = leaf
			}
		}
		for _, a := range c.Aliases {
			a = strings.TrimSpace(a)
			if a == "" || strings.Contains(a, " ") {
				continue
			}
			alias[a] = leaf
		}
	}

	cb := map[string]map[string]CallbackRoute{}
	for _, r := range cbs {
		p := strings.TrimSpace(r.Plugin)
		a := strings.TrimSpace(r.Action)
		if p == "" || a == "" || r.Handle == nil {
			continue
		}
		if cb[p] == nil {
			cb[p] = map[string]CallbackRoute{}
		}
		cb[p][a] = r
	}

	m.mu.Lock()
	m.root = root
	m.alias = alias
	m.mu.Unlock()

	m.cbMu.Lock()
	m.callbacks = cb
	m.cbMu.Unlock()
}

func (m *CommandManager) DispatchLoop(ctx context.Context, updates <-chan kit.Update) error {
	// bounded worker pool (hemat resource & goroutine safe)
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}

	if m.log != nil {
		m.log.Info("command dispatcher started", slog.Int("workers", workers), slog.Int("job_queue_cap", cap(m.jobs)))
	}

	var (
		wg        sync.WaitGroup
		closeOnce sync.Once
	)

	closeJobs := func() {
		closeOnce.Do(func() {
			close(m.jobs)
		})
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		idx := i
		go func() {
			defer wg.Done()
			if m.log != nil {
				m.log.Debug("command worker started", slog.Int("worker", idx))
			}
			defer func() {
				if r := recover(); r != nil {
					if m.log != nil {
						m.log.Error("panic in command worker", slog.Int("worker", idx), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
					}
				}
				if m.log != nil {
					m.log.Debug("command worker stopped", slog.Int("worker", idx))
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-m.jobs:
					if !ok {
						return
					}
					if job == nil {
						continue
					}
					job()
				}
			}
		}()
	}

	defer func() {
		closeJobs()
		wg.Wait()
		if m.log != nil {
			m.log.Info("command dispatcher stopped")
		}
	}()

	for {
		select {
		case <-ctx.Done():
			if m.log != nil {
				m.log.Info("command dispatcher stopped", slog.Any("err", ctx.Err()))
			}
			return nil
		case up, ok := <-updates:
			if !ok {
				if m.log != nil {
					m.log.Info("command dispatcher stopped (updates channel closed)")
				}
				return nil
			}
			m.routeUpdate(ctx, up)
		}
	}
}

func (m *CommandManager) routeUpdate(root context.Context, up kit.Update) {
	switch up.Kind {
	case kit.UpdateMessage:
		m.routeMessage(root, up)
	case kit.UpdateCallback:
		m.routeCallback(root, up)
	}
}

func (m *CommandManager) routeMessage(root context.Context, up kit.Update) {
	if up.Message == nil {
		return
	}
	msg := up.Message
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return
	}

	parts := tokenizeCommandLine(text)
	if len(parts) == 0 {
		return
	}
	word := strings.TrimPrefix(parts[0], "/")
	if i := strings.IndexByte(word, '@'); i >= 0 {
		word = word[:i]
	}
	args := []string{}
	if len(parts) > 1 {
		args = parts[1:]
	}

	// snapshot registry
	m.mu.RLock()
	rootNode := m.root
	aliasMap := m.alias
	m.mu.RUnlock()

	// alias as root-level shortcut
	if leaf, ok := aliasMap[word]; ok && leaf != nil && leaf.cmd != nil {
		cmd := *leaf.cmd
		pos, flags, bools := parseFlags(args)
		m.enqueueCommand(root, up, cmd, splitRoute(cmd.Route), pos, args, flags, bools)
		return
	}

	// traverse subcommand tree
	cur, ok := rootNode.child(word)
	if !ok {
		_, _ = m.adapter.SendText(root, kit.ChatTarget{ChatID: msg.ChatID, ThreadID: msg.ThreadID}, "unknown command. try /help", nil)
		return
	}
	path := []string{word}
	for len(args) > 0 {
		nxt := args[0]
		if strings.HasPrefix(nxt, "-") { // flags start, stop subcommand traversal
			break
		}
		child, ok := cur.child(nxt)
		if !ok {
			break
		}
		cur = child
		path = append(path, nxt)
		args = args[1:]
	}

	// If container node without handler: show help for that path
	if cur.cmd == nil {
		txt := m.helpText(path)
		_, _ = m.adapter.SendText(root, kit.ChatTarget{ChatID: msg.ChatID, ThreadID: msg.ThreadID}, txt, &kit.SendOptions{DisablePreview: true})
		return
	}

	cmd := *cur.cmd
	pos, flags, bools := parseFlags(args)
	m.enqueueCommand(root, up, cmd, path, pos, args, flags, bools)
}

func (m *CommandManager) enqueueCommand(root context.Context, up kit.Update, cmd Command, path []string, args []string, raw []string, flags map[string]string, bools map[string]bool) {
	msg := up.Message
	if msg == nil {
		return
	}

	owners := m.ownersSnapshot()
	if cmd.Access == AccessOwnerOnly && !isOwner(msg.FromID, owners) {
		_, _ = m.adapter.SendText(root, kit.ChatTarget{ChatID: msg.ChatID, ThreadID: msg.ThreadID}, "unauthorized", nil)
		return
	}

	rid := newReqID()
	reqLog := m.log.With(
		slog.String("rid", rid),
		slog.Int64("chat_id", msg.ChatID),
		slog.Int("thread_id", msg.ThreadID),
		slog.Int64("from_id", msg.FromID),
		slog.String("cmd", cmd.Route),
	)

	req := &Request{
		Update:      up,
		Chat:        kit.ChatTarget{ChatID: msg.ChatID, ThreadID: msg.ThreadID},
		FromID:      msg.FromID,
		Path:        path,
		Command:     cmd.Route,
		Args:        args,
		RawArgs:     raw,
		Flags:       flags,
		BoolFlags:   bools,
		ReqID:       rid,
		Adapter:     m.adapter,
		Config:      m.cfgm.Get(),
		Logger:      reqLog,
		Services:    m.serv,
		OwnerUserID: owners,
	}

	final := Chain(
		cmd.Handle,
		MWPanicRecover(m.log),
		MWRequestLog(m.log),
		MWTimeout(cmd.Timeout),
	)

	select {
	case m.jobs <- func() { _ = final(root, req) }:
	default:
		_, _ = m.adapter.SendText(root, req.Chat, "busy, try again", nil)
	}
}

func (m *CommandManager) routeCallback(root context.Context, up kit.Update) {
	if up.Callback == nil {
		return
	}
	cb := up.Callback
	data := strings.TrimSpace(cb.Data)
	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 2 {
		return
	}
	plugin := parts[0]
	action := parts[1]
	payload := ""
	if len(parts) == 3 {
		payload = parts[2]
	}

	m.cbMu.RLock()
	actions := m.callbacks[plugin]
	route, ok := actions[action]
	m.cbMu.RUnlock()
	if !ok {
		return
	}

	// Owner check: let plugin enforce if needed (or add in route later)
	rid := newReqID()
	reqLog := m.log.With(
		slog.String("rid", rid),
		slog.Int64("chat_id", cb.ChatID),
		slog.Int("thread_id", cb.ThreadID),
		slog.Int64("from_id", cb.FromID),
		slog.String("cmd", "cb:"+plugin+":"+action),
	)
	req := &Request{
		Update:      up,
		Chat:        kit.ChatTarget{ChatID: cb.ChatID, ThreadID: cb.ThreadID},
		FromID:      cb.FromID,
		Command:     "cb:" + plugin + ":" + action,
		Args:        nil,
		Payload:     payload,
		RawArgs:     nil,
		Flags:       nil,
		BoolFlags:   nil,
		ReqID:       rid,
		Adapter:     m.adapter,
		Config:      m.cfgm.Get(),
		Logger:      reqLog,
		Services:    m.serv,
		OwnerUserID: m.ownersSnapshot(),
	}

	h := func(ctx context.Context, r *Request) error { return route.Handle(ctx, r, payload) }

	final := Chain(
		h,
		MWPanicRecover(m.log),
		MWRequestLog(m.log),
		MWTimeout(route.Timeout),
	)

	select {
	case m.jobs <- func() {
		_ = final(root, req)
		// best-effort to stop "loading" UI
		_ = m.adapter.AnswerCallback(root, cb.ID, "")
	}:
	default:
		_ = m.adapter.AnswerCallback(root, cb.ID, "busy")
	}
}

func isOwner(id int64, owners []int64) bool {
	for _, o := range owners {
		if o == id {
			return true
		}
	}
	return false
}
