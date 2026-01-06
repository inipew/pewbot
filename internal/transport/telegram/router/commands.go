package router

import (
	"context"
	logx "pewbot/pkg/logx"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	kit "pewbot/internal/transport"
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

// CallbackAccess controls who can trigger an inline-button callback.
//
// Default is owner-only (safer for operational actions in groups).
// Set CallbackAccessEveryone explicitly for public UI callbacks.
type CallbackAccess int

const (
	CallbackAccessOwnerOnly CallbackAccess = iota
	CallbackAccessEveryone
)

type CallbackRoute struct {
	Plugin      string
	Action      string
	Description string
	Access      CallbackAccess
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
	Logger      logx.Logger
	Services    *Services
	OwnerUserID []int64
}

type Services struct {
	Scheduler SchedulerPort
	Notifier  NotifierPort
	Plugins   PluginsPort

	// AppSupervisor is set by the app once started.
	// It can be nil in minimal/test environments.
	AppSupervisor *Supervisor

	// RuntimeSupervisors exposes additional subsystem supervisors (adapter, engine, etc.)
	// for operational commands like /health.
	//
	// This is read-only / best-effort; entries may be nil in minimal/test environments.
	RuntimeSupervisors *SupervisorRegistry
}

// PluginsPort exposes read-only plugin runtime state for operational commands.
//
// Note: plugins are still in-process; this is not a security boundary.
type PluginsPort interface {
	// Snapshot returns the current plugin runtime state and the last known health
	// result (if available).
	Snapshot() PluginsSnapshot

	// CheckHealth triggers an on-demand health check for the given plugins.
	// If names is empty, it checks all running plugins that implement HealthChecker.
	CheckHealth(ctx context.Context, names []string) []PluginHealthResult
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

	log     logx.Logger
	adapter kit.Adapter
	cfgm    *ConfigManager
	serv    *Services

	runMu   sync.Mutex
	running bool
	sup     *Supervisor

	jobs chan func()
}

func NewCommandManager(log logx.Logger, adapter kit.Adapter, cfgm *ConfigManager, serv *Services, owners []int64) *CommandManager {
	if log.IsZero() {
		log = logx.Nop()
	}
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

// Supervisor returns the command manager's internal supervisor (nil if not running).
// Useful for operational visibility (/health).
func (m *CommandManager) Supervisor() *Supervisor {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if !m.running {
		return nil
	}
	return m.sup
}

func (m *CommandManager) setSupervisor(sup *Supervisor, running bool) {
	m.runMu.Lock()
	m.sup = sup
	m.running = running
	m.runMu.Unlock()
}

// tryEnqueue is a panic-safe enqueue helper (handles the jobs channel being closed).
func (m *CommandManager) tryEnqueue(fn func()) (ok bool) {
	if fn == nil {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	select {
	case m.jobs <- fn:
		return true
	default:
		return false
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
		Description: "tampilkan bantuan",
		Usage:       "/help [cmd] [sub...]",
		Access:      AccessEveryone,
		Handle: func(ctx context.Context, req *Request) error {
			text := m.helpText(req.Args)
			_, _ = req.Adapter.SendText(ctx, req.Chat, text, &kit.SendOptions{DisablePreview: true, ParseMode: "HTML"})
			return nil
		},
	}
	cmds = append(cmds, helper)

	root := newRoot()
	alias := map[string]*cmdNode{}
	menuCandidates := make([]Command, 0, len(cmds))

	for _, c := range cmds {
		route := splitRoute(c.Route)
		if len(route) == 0 || c.Handle == nil {
			continue
		}
		cc := c // copy
		root.add(route, cc)
		menuCandidates = append(menuCandidates, cc)

		leaf := root.find(route)
		// Auto aliases to support Telegram /menu autocomplete.
		// Telegram command names are restricted to [a-z0-9_]{1,32}.
		//
		// IMPORTANT:
		// Do NOT add the canonical single-token command name itself (e.g. "systemd")
		// into the alias map, because that would short-circuit subcommand traversal:
		// "/systemd status" would be treated as an alias hit for "systemd" and never
		// reach the "systemd status" route.
		//
		// We only add an auto-alias when it is *actually different* from the base
		// command token (multi-token routes, or single-token routes that require
		// sanitization such as "a-b" -> "a_b").
		if leaf != nil {
			if menu, ok := telegramCommandNameFromRoute(route); ok {
				if len(route) > 1 || (len(route) == 1 && menu != route[0]) {
					if _, exists := alias[menu]; !exists {
						alias[menu] = leaf
					}
				}
			}
		}
		for _, a := range c.Aliases {
			a = strings.TrimSpace(a)
			if a == "" || strings.Contains(a, " ") {
				continue
			}
			alias[a] = leaf
			// also add a Telegram-safe alias variant when needed (best-effort)
			if sa := sanitizeTelegramCommand(a); sa != "" {
				if _, exists := alias[sa]; !exists {
					alias[sa] = leaf
				}
			}
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

	// Best-effort Telegram /menu autocomplete update (non-blocking).
	if up, ok := m.adapter.(kit.CommandMenuUpdater); ok {
		menu := buildTelegramMenuCommands(root, menuCandidates)
		run := func(parent context.Context) {
			ctx, cancel := context.WithTimeout(parent, 5*time.Second)
			defer cancel()
			_ = up.UpdateMenuCommands(ctx, menu)
		}

		// Prefer running under the app supervisor so it's canceled cleanly on shutdown.
		if m.serv != nil && m.serv.AppSupervisor != nil {
			m.serv.AppSupervisor.Go("telegram.menu.update", func(ctx context.Context) error {
				run(ctx)
				return nil
			})
		} else {
			// Fallback for minimal/test environments.
			go run(context.Background())
		}
	}
}

func (m *CommandManager) DispatchLoop(ctx context.Context, updates <-chan kit.Update) error {
	// bounded worker pool (hemat resource & goroutine safe)
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}

	// Internal supervisor keeps the worker pool resilient and observable.
	sup := NewSupervisor(ctx,
		WithLogger(m.log.With(logx.String("comp", "telegram.router"))),
		WithCancelOnError(false),
	)
	m.setSupervisor(sup, true)
	if m.serv != nil && m.serv.RuntimeSupervisors != nil {
		m.serv.RuntimeSupervisors.Set("telegram.router", sup)
	}

	if !m.log.IsZero() {
		m.log.Info("command dispatcher started", logx.Int("workers", workers), logx.Int("job_queue_cap", cap(m.jobs)))
	}

	var closeOnce sync.Once
	closeJobs := func() {
		closeOnce.Do(func() {
			// Mark as not running before closing so enqueue can degrade gracefully.
			m.setSupervisor(sup, false)
			close(m.jobs)
		})
	}

	for i := 0; i < workers; i++ {
		idx := i
		name := "command.worker." + strconv.Itoa(idx)
		sup.GoRestart(name, func(c context.Context) error {
			if !m.log.IsZero() {
				m.log.Debug("command worker started", logx.Int("worker", idx))
			}
			defer func() {
				if !m.log.IsZero() {
					m.log.Debug("command worker stopped", logx.Int("worker", idx))
				}
			}()
			for {
				select {
				case <-c.Done():
					return nil
				case job, ok := <-m.jobs:
					if !ok {
						return nil
					}
					if job == nil {
						continue
					}
					// Defensive: a job should never panic (middleware already catches),
					// but keep workers alive if it happens.
					func() {
						defer func() {
							if r := recover(); r != nil {
								m.log.Error("panic in command job", logx.Int("worker", idx), logx.Any("panic", r), logx.String("stack", string(debug.Stack())))
							}
						}()
						job()
					}()
				}
			}
		},
			WithRestartBackoff(200*time.Millisecond, 5*time.Second),
			WithPublishFirstError(true),
			WithStopOnCleanExit(true),
		)
	}

	defer func() {
		closeJobs()
		// Wait briefly for workers to drain.
		wctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = sup.Wait(wctx)
		cancel()
		if m.serv != nil && m.serv.RuntimeSupervisors != nil {
			m.serv.RuntimeSupervisors.Delete("telegram.router")
		}
		m.setSupervisor(nil, false)
		if !m.log.IsZero() {
			m.log.Info("command dispatcher stopped")
		}
	}()

	for {
		select {
		case <-ctx.Done():
			if !m.log.IsZero() {
				m.log.Info("command dispatcher stopped", logx.Any("err", ctx.Err()))
			}
			return nil
		case up, ok := <-updates:
			if !ok {
				if !m.log.IsZero() {
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
		_, _ = m.adapter.SendText(root, kit.ChatTarget{ChatID: msg.ChatID, ThreadID: msg.ThreadID}, "perintah tidak dikenal. coba /help", nil)
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
		_, _ = m.adapter.SendText(root, kit.ChatTarget{ChatID: msg.ChatID, ThreadID: msg.ThreadID}, txt, &kit.SendOptions{DisablePreview: true, ParseMode: "HTML"})
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
		logx.String("rid", rid),
		logx.Int64("chat_id", msg.ChatID),
		logx.Int("thread_id", msg.ThreadID),
		logx.Int64("from_id", msg.FromID),
		logx.String("cmd", cmd.Route),
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

	if !m.tryEnqueue(func() { _ = final(root, req) }) {
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

	// Access control: default is owner-only.
	owners := m.ownersSnapshot()
	if route.Access == CallbackAccessOwnerOnly && !isOwner(cb.FromID, owners) {
		_ = m.adapter.AnswerCallback(root, cb.ID, "forbidden")
		return
	}
	rid := newReqID()
	reqLog := m.log.With(
		logx.String("rid", rid),
		logx.Int64("chat_id", cb.ChatID),
		logx.Int("thread_id", cb.ThreadID),
		logx.Int64("from_id", cb.FromID),
		logx.String("cmd", "cb:"+plugin+":"+action),
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
		OwnerUserID: owners,
	}

	h := func(ctx context.Context, r *Request) error { return route.Handle(ctx, r, payload) }

	final := Chain(
		h,
		MWPanicRecover(m.log),
		MWRequestLog(m.log),
		MWTimeout(route.Timeout),
	)

	if !m.tryEnqueue(func() {
		_ = final(root, req)
		// best-effort to stop "loading" UI
		_ = m.adapter.AnswerCallback(root, cb.ID, "")
	}) {
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
