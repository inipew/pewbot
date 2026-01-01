package echo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"

	"pewbot/internal/core"
	"pewbot/internal/kit"
	"pewbot/pkg/tgui"
)

type Config struct {
	Prefix string `json:"prefix"`
}

type Plugin struct {
	log  *slog.Logger
	cfg  Config
	deps core.PluginDeps
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "echo" }

func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error {
	p.deps = deps
	p.log = deps.Logger.With(slog.String("plugin", p.Name()))
	return nil
}

func (p *Plugin) OnConfigChange(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	p.cfg = c
	return nil
}

func (p *Plugin) Start(ctx context.Context) error { return nil }
func (p *Plugin) Stop(ctx context.Context) error  { return nil }

func (p *Plugin) Commands() []core.Command {
	return []core.Command{
		{
			Route:       "echo",
			Description: "echo back text",
			Usage:       "/echo <text>",
			Access:      core.AccessEveryone,
			Handle: func(ctx context.Context, req *core.Request) error {
				txt := strings.Join(req.Args, " ")
				if txt == "" {
					txt = "(empty)"
				}
				_, _ = req.Adapter.SendText(ctx, req.Chat, p.cfg.Prefix+txt, nil)
				return nil
			},
		},
		{
			Route:       "echo ui",
			Aliases:     []string{"menu"},
			Description: "interactive echo (inline buttons + callbacks)",
			Usage:       "/menu <text>  OR  /echo ui <text>",
			Access:      core.AccessEveryone,
			Handle: func(ctx context.Context, req *core.Request) error {
				txt := strings.Join(req.Args, " ")
				if strings.TrimSpace(txt) == "" {
					txt = "hello world"
				}
				payload := base64.RawURLEncoding.EncodeToString([]byte(txt))
				upper := "echo:upper:" + payload
				lower := "echo:lower:" + payload
				rm := tgui.NewInline().
					Row(tgui.Btn("⬆️ Upper", upper), tgui.Btn("⬇️ Lower", lower)).
					Markup()
				_, _ = req.Adapter.SendText(ctx, req.Chat, "Pilih transform:", &kit.SendOptions{ReplyMarkupAdapter: rm})
				return nil
			},
		},
	}
}

func (p *Plugin) Callbacks() []core.CallbackRoute {
	return []core.CallbackRoute{
		{
			Action:      "upper",
			Description: "uppercase text",
			Handle: func(ctx context.Context, req *core.Request, payload string) error {
				b, err := base64.RawURLEncoding.DecodeString(payload)
				if err != nil {
					return nil
				}
				out := strings.ToUpper(string(b))
				ref := kit.MessageRef{ChatID: req.Chat.ChatID, ThreadID: req.Chat.ThreadID, MessageID: req.Update.Callback.MessageID}
				return req.Adapter.EditText(ctx, ref, p.cfg.Prefix+out, nil)
			},
		},
		{
			Action:      "lower",
			Description: "lowercase text",
			Handle: func(ctx context.Context, req *core.Request, payload string) error {
				b, err := base64.RawURLEncoding.DecodeString(payload)
				if err != nil {
					return nil
				}
				out := strings.ToLower(string(b))
				ref := kit.MessageRef{ChatID: req.Chat.ChatID, ThreadID: req.Chat.ThreadID, MessageID: req.Update.Callback.MessageID}
				return req.Adapter.EditText(ctx, ref, p.cfg.Prefix+out, nil)
			},
		},
	}
}
