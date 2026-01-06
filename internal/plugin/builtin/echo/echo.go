package echo

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	core "pewbot/internal/plugin"
	"pewbot/internal/plugin/kit"
	"pewbot/pkg/tgui"
)

type Config struct {
	Prefix string `json:"prefix"`
}

type Plugin struct {
	pluginkit.EnhancedPluginBase

	mu  sync.RWMutex
	cfg Config
	ui  *pluginkit.UIHub
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "echo" }

func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error {
	p.InitEnhanced(deps, p.Name())
	// defaults
	p.mu.Lock()
	p.cfg = Config{Prefix: "echo: "}
	p.mu.Unlock()

	// UI hub (single callback action) + token store for long payloads.
	p.ui = pluginkit.NewUIHub(p.Name()).WithAccess(core.CallbackAccessEveryone)
	p.ui.On("menu", p.viewMenu)
	p.ui.On("upper", p.viewUpper)
	p.ui.On("lower", p.viewLower)
	p.ui.On("full", p.viewFull)
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	p.StartEnhanced(ctx)
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	return p.StopEnhanced(ctx)
}

func (p *Plugin) OnConfigChange(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	if strings.TrimSpace(c.Prefix) == "" {
		c.Prefix = "echo: "
	}
	p.mu.Lock()
	p.cfg = c
	p.mu.Unlock()
	return nil
}

func (p *Plugin) cfgSnapshot() Config {
	p.mu.RLock()
	c := p.cfg
	p.mu.RUnlock()
	return c
}

func (p *Plugin) Commands() []core.Command {
	return []core.Command{
		{
			Route:       "echo",
			Description: "balas ulang teks yang kamu kirim",
			Usage:       "/echo <text>",
			Access:      core.AccessEveryone,
			Handle: func(ctx context.Context, req *core.Request) error {
				c := p.cfgSnapshot()
				txt := strings.Join(req.Args, " ")
				if txt == "" {
					txt = "(empty)"
				}
				_, _ = req.Adapter.SendText(ctx, req.Chat, c.Prefix+txt, nil)
				return nil
			},
		},
		{
			Route:       "echo ui",
			Aliases:     []string{"menu"},
			Description: "echo interaktif (tombol inline)",
			Usage:       "/echo ui <text>  (atau /menu <text>)",
			Access:      core.AccessEveryone,
			Handle: func(ctx context.Context, req *core.Request) error {
				txt := strings.Join(req.Args, " ")
				if strings.TrimSpace(txt) == "" {
					txt = "hello world"
				}

				// Store the text server-side and keep callback_data small.
				tok := ""
				if p.ui != nil && p.ui.Store() != nil {
					tok = p.ui.Store().PutString(txt)
				}
				st := pluginkit.UIState{View: "menu", Key: tok}
				msg, _ := p.viewMenu(ctx, req, st)
				_, _ = msg.Send(ctx, req.Adapter, req.Chat)
				return nil
			},
		},
	}
}

func (p *Plugin) Callbacks() []core.CallbackRoute {
	if p.ui == nil {
		return nil
	}
	return []core.CallbackRoute{p.ui.Route()}
}

func (p *Plugin) viewMenu(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	c := p.cfgSnapshot()
	val := "(expired)"
	if p.ui != nil && p.ui.Store() != nil {
		if s, ok := p.ui.Store().GetString(st.Key); ok {
			val = s
		}
	}
	preview := tgui.TruncRunes(val, 160)
	if preview == "" {
		preview = "(empty)"
	}

	kb := tgui.NewInline().
		Row(
			p.ui.Button("⬆️ Upper", pluginkit.UIState{View: "upper", Key: st.Key}),
			p.ui.Button("⬇️ Lower", pluginkit.UIState{View: "lower", Key: st.Key}),
		).
		Row(
			p.ui.Button("📝 Full", pluginkit.UIState{View: "full", Key: st.Key}),
			p.ui.Button("🔄 Refresh", pluginkit.UIState{View: "menu", Key: st.Key}),
		)

	msg := tgui.New().
		Title("🎛️", "Echo UI").
		Line(c.Prefix + "Pilih transform:").
		Line("Preview:").
		Code(preview).
		Inline(kb).
		Build()
	return msg, nil
}

func (p *Plugin) viewUpper(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	c := p.cfgSnapshot()
	val := ""
	if p.ui != nil && p.ui.Store() != nil {
		if s, ok := p.ui.Store().GetString(st.Key); ok {
			val = s
		}
	}
	out := strings.ToUpper(val)
	preview := tgui.TruncRunes(out, 600)

	kb := tgui.NewInline().
		Row(
			p.ui.Button("↩️ Menu", pluginkit.UIState{View: "menu", Key: st.Key}),
			p.ui.Button("⬇️ Lower", pluginkit.UIState{View: "lower", Key: st.Key}),
		).
		Row(
			p.ui.Button("📝 Full", pluginkit.UIState{View: "full", Key: st.Key}),
		)

	msg := tgui.New().
		Title("⬆️", "Uppercase").
		Line(c.Prefix + "Result:").
		Code(preview).
		Inline(kb).
		Build()
	return msg, nil
}

func (p *Plugin) viewLower(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	c := p.cfgSnapshot()
	val := ""
	if p.ui != nil && p.ui.Store() != nil {
		if s, ok := p.ui.Store().GetString(st.Key); ok {
			val = s
		}
	}
	out := strings.ToLower(val)
	preview := tgui.TruncRunes(out, 600)

	kb := tgui.NewInline().
		Row(
			p.ui.Button("↩️ Menu", pluginkit.UIState{View: "menu", Key: st.Key}),
			p.ui.Button("⬆️ Upper", pluginkit.UIState{View: "upper", Key: st.Key}),
		).
		Row(
			p.ui.Button("📝 Full", pluginkit.UIState{View: "full", Key: st.Key}),
		)

	msg := tgui.New().
		Title("⬇️", "Lowercase").
		Line(c.Prefix + "Result:").
		Code(preview).
		Inline(kb).
		Build()
	return msg, nil
}

func (p *Plugin) viewFull(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	c := p.cfgSnapshot()
	val := "(expired)"
	if p.ui != nil && p.ui.Store() != nil {
		if s, ok := p.ui.Store().GetString(st.Key); ok {
			val = s
		}
	}

	kb := tgui.NewInline().
		Row(
			p.ui.Button("↩️ Menu", pluginkit.UIState{View: "menu", Key: st.Key}),
			p.ui.Button("⬆️ Upper", pluginkit.UIState{View: "upper", Key: st.Key}),
			p.ui.Button("⬇️ Lower", pluginkit.UIState{View: "lower", Key: st.Key}),
		)

	msg := tgui.New().
		Title("📝", "Full Text").
		Line(c.Prefix + "Text:").
		PreMulti(val).
		Inline(kb).
		Build()
	return msg, nil
}
