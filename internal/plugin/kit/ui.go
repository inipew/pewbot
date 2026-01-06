package pluginkit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	tele "gopkg.in/telebot.v4"
	core "pewbot/internal/plugin"
	kit "pewbot/internal/transport"
	"pewbot/pkg/tgui"
)

// UIState is a small, callback-safe state object for UI navigation.
// Keep it tiny; Telegram callback_data is limited to 64 bytes.
type UIState struct {
	View string `json:"v"`           // view id
	Key  string `json:"k,omitempty"` // optional token/id
	Page int    `json:"p,omitempty"`
	Size int    `json:"s,omitempty"`
}

type UIView func(ctx context.Context, req *core.Request, st UIState) (tgui.Message, error)

// UIHub provides a small, ergonomic “center” to render & update Telegram UI.
//
// How it works:
//  1. You register views (string -> renderer).
//  2. You expose exactly one callback route (Action default: "ui").
//  3. Buttons carry UIState (auto-packed + size-safe via TokenStore fallback).
//  4. On press, UIHub edits the originating message with the next view.
type UIHub struct {
	plugin string
	action string

	views map[string]UIView
	store *tgui.TokenStore

	access  core.CallbackAccess
	timeout time.Duration
}

func NewUIHub(plugin string) *UIHub {
	return &UIHub{
		plugin: plugin,
		action: "ui",
		views:  map[string]UIView{},
		store:  tgui.NewTokenStore(),
		access: core.CallbackAccessOwnerOnly,
	}
}

func (u *UIHub) WithAccess(a core.CallbackAccess) *UIHub {
	if u != nil {
		u.access = a
	}
	return u
}

func (u *UIHub) WithTimeout(d time.Duration) *UIHub {
	if u != nil {
		u.timeout = d
	}
	return u
}

// WithAction changes the callback action name (default: "ui").
func (u *UIHub) WithAction(action string) *UIHub {
	if u != nil {
		action = strings.TrimSpace(action)
		if action != "" {
			u.action = action
		}
	}
	return u
}

func (u *UIHub) Store() *tgui.TokenStore {
	if u == nil {
		return nil
	}
	return u.store
}

// On registers a view renderer.
func (u *UIHub) On(view string, h UIView) *UIHub {
	if u == nil {
		return u
	}
	view = strings.TrimSpace(view)
	if view == "" || h == nil {
		return u
	}
	u.views[view] = h
	return u
}

// Route returns a single callback route that dispatches to registered views.
func (u *UIHub) Route() core.CallbackRoute {
	return core.CallbackRoute{
		Plugin:      u.plugin,
		Action:      u.action,
		Description: "UI hub",
		Access:      u.access,
		Timeout:     u.timeout,
		Handle:      u.handle,
	}
}

// Button builds a callback button that navigates to st.
// It automatically falls back to TokenStore if packed payload exceeds limit.
func (u *UIHub) Button(text string, st UIState) tele.Btn {
	if u == nil {
		return tele.Btn{Text: text}
	}
	data, err := tgui.ActionDataWithStore(u.plugin, u.action, st, u.store)
	if err != nil {
		data = tgui.Data(u.plugin, u.action, "")
	}
	return tele.Btn{Text: text, Data: data}
}

func (u *UIHub) handle(ctx context.Context, req *core.Request, payload string) error {
	st, err := u.decodeState(payload)
	if err != nil {
		return u.editErr(ctx, req, err)
	}
	view := strings.TrimSpace(st.View)
	if view == "" {
		return u.editErr(ctx, req, errors.New("tgui: missing view"))
	}
	h := u.views[view]
	if h == nil {
		return u.editErr(ctx, req, errors.New("tgui: unknown view: "+view))
	}

	msg, err := h(ctx, req, st)
	if err != nil {
		return u.editErr(ctx, req, err)
	}

	cb := req.Update.Callback
	if cb == nil {
		// If called outside of callback context, fall back to send.
		_, _ = msg.Send(ctx, req.Adapter, req.Chat)
		return nil
	}
	ref := kit.MessageRef{ChatID: cb.ChatID, ThreadID: cb.ThreadID, MessageID: cb.MessageID}
	return msg.Edit(ctx, req.Adapter, ref, req.Chat)
}

func (u *UIHub) decodeState(payload string) (UIState, error) {
	var st UIState
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return st, errors.New("tgui: empty payload")
	}
	// TokenStore fallback: payload token begins with "~".
	if strings.HasPrefix(payload, "~") && u != nil && u.store != nil {
		b, ok := u.store.GetBytes(payload)
		if !ok {
			return st, errors.New("tgui: payload expired")
		}
		if err := json.Unmarshal(b, &st); err != nil {
			return st, err
		}
		return st, nil
	}
	// Regular path: base64url(JSON).
	if err := tgui.UnpackJSON(payload, &st); err != nil {
		return st, err
	}
	return st, nil
}

func (u *UIHub) editErr(ctx context.Context, req *core.Request, err error) error {
	if err == nil {
		return nil
	}
	msg := tgui.New().
		Title("⚠️", "UI Error").
		Line(err.Error()).
		Build()
	cb := req.Update.Callback
	if cb == nil {
		_, _ = msg.Send(ctx, req.Adapter, req.Chat)
		return nil
	}
	ref := kit.MessageRef{ChatID: cb.ChatID, ThreadID: cb.ThreadID, MessageID: cb.MessageID}
	return msg.Edit(ctx, req.Adapter, ref, req.Chat)
}
