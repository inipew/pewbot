package systemd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tele "gopkg.in/telebot.v4"

	core "pewbot/internal/plugin"
	"pewbot/internal/plugin/kit"
	sm "pewbot/pkg/systemdmanager"
	"pewbot/pkg/tgui"
)

const (
	viewStatusList = "status_list"
	viewStatusUnit = "status_unit"

	viewPickStart   = "start_pick"
	viewPickStop    = "stop_pick"
	viewPickRestart = "restart_pick"
	viewPickEnable  = "enable_pick"
	viewPickDisable = "disable_pick"

	viewConfirmStart   = "start_confirm"
	viewConfirmStop    = "stop_confirm"
	viewConfirmRestart = "restart_confirm"
	viewConfirmEnable  = "enable_confirm"
	viewConfirmDisable = "disable_confirm"

	viewExecStart   = "start_exec"
	viewExecStop    = "stop_exec"
	viewExecRestart = "restart_exec"
	viewExecEnable  = "enable_exec"
	viewExecDisable = "disable_exec"

	viewClosed = "closed"
)

func pickViewForAction(action string) string {
	switch strings.TrimSpace(action) {
	case "start":
		return viewPickStart
	case "stop":
		return viewPickStop
	case "restart":
		return viewPickRestart
	case "enable":
		return viewPickEnable
	case "disable":
		return viewPickDisable
	default:
		return viewPickRestart
	}
}

func confirmViewForAction(action string) string {
	switch strings.TrimSpace(action) {
	case "start":
		return viewConfirmStart
	case "stop":
		return viewConfirmStop
	case "restart":
		return viewConfirmRestart
	case "enable":
		return viewConfirmEnable
	case "disable":
		return viewConfirmDisable
	default:
		return viewConfirmRestart
	}
}

func execViewForAction(action string) string {
	switch strings.TrimSpace(action) {
	case "start":
		return viewExecStart
	case "stop":
		return viewExecStop
	case "restart":
		return viewExecRestart
	case "enable":
		return viewExecEnable
	case "disable":
		return viewExecDisable
	default:
		return viewExecRestart
	}
}

func opTitle(action string) (emoji, title, verb string) {
	switch strings.TrimSpace(action) {
	case "start":
		return "▶️", "Start service", "start"
	case "stop":
		return "⏹", "Stop service", "stop"
	case "restart":
		return "🔁", "Restart service", "restart"
	case "enable":
		return "✅", "Enable on boot", "enable"
	case "disable":
		return "🚫", "Disable on boot", "disable"
	default:
		return "🔧", "systemd", action
	}
}

func normalizeUnit(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	// Keep behavior consistent with normalizeUnits() used by config.
	out := normalizeUnits([]string{u})
	if len(out) == 0 {
		return ""
	}
	return out[0]
}

func (p *Plugin) renderView(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	switch st.View {
	case viewPickStart:
		return p.viewPickStart(ctx, req, st)
	case viewPickStop:
		return p.viewPickStop(ctx, req, st)
	case viewPickRestart:
		return p.viewPickRestart(ctx, req, st)
	case viewPickEnable:
		return p.viewPickEnable(ctx, req, st)
	case viewPickDisable:
		return p.viewPickDisable(ctx, req, st)
	default:
		// Best-effort fallback.
		return p.viewPickRestart(ctx, req, st)
	}
}

// --- Status views ---

func (p *Plugin) viewStatusList(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	mgr := p.mgrSnapshot()
	ctx2, cancel := p.opCtx(ctx)
	defer cancel()

	if mgr == nil {
		kb := tgui.NewInline().Row(p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}))
		return tgui.New().Title("⚠️", "systemd").Line("systemd manager not available (non-linux/dbus permission?)").Inline(kb).Build(), nil
	}

	statuses, err := mgr.GetAllStatusContext(ctx2)
	if err != nil {
		kb := tgui.NewInline().Row(p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}))
		return tgui.New().Title("⚠️", "systemd status").Line("failed: " + err.Error()).Inline(kb).Build(), nil
	}
	if len(statuses) == 0 {
		kb := tgui.NewInline().Row(p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}))
		return tgui.New().Title("🖥️", "systemd status").Line("no managed units configured").Inline(kb).Build(), nil
	}

	// Sort for stable UI.
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })

	// Summary counters (for the whole managed set, not just this page).
	var nActive, nFailed, nInactive, nOther int
	for _, s := range statuses {
		a := strings.ToLower(strings.TrimSpace(s.Active))
		sub := strings.ToLower(strings.TrimSpace(s.SubState))
		switch {
		case a == "active" && (sub == "running" || sub == "listening" || sub == "exited"):
			nActive++
		case a == "failed":
			nFailed++
		case a == "inactive":
			nInactive++
		default:
			nOther++
		}
	}

	sub, page, size, _, _, hasPrev, hasNext := tgui.PaginateSlice(statuses, st.Page, st.Size)

	b := tgui.New().Title("🛠️", "Service Status")

	// Detail tree list (no <pre>), clean and background-free.
	for _, ln := range statusDetailLines(sub) {
		b.Line(ln)
	}

	sum := fmt.Sprintf("%d active • %d inactive • %d failed", nActive, nInactive, nFailed)
	if nOther > 0 {
		sum += fmt.Sprintf(" • %d other", nOther)
	}
	b.RawLine("📊 <b>Summary</b>: " + tgui.Esc(sum).String())
	b.RawLine("🕒 <b>Updated</b>: " + tgui.Esc(tsNowShort()).String() + "  •  " + tgui.Esc(pageShort(page, size, len(statuses))).String())

	// Keyboard
	cols := 3
	btns := make([]tele.Btn, 0, len(sub))
	for _, s := range sub {
		btns = append(btns, p.ui.Button(stateChar(s.Active, s.SubState)+" "+s.Name, pluginkit.UIState{View: viewStatusUnit, Key: s.Name}))
	}

	kb := tgui.NewInline()
	for i := 0; i < len(btns); i += cols {
		end := i + cols
		if end > len(btns) {
			end = len(btns)
		}
		kb.Row(btns[i:end]...)
	}
	if hasPrev || hasNext {
		row := make([]tele.Btn, 0, 2)
		if hasPrev {
			row = append(row, p.ui.Button("⬅️ Prev", pluginkit.UIState{View: viewStatusList, Page: page - 1, Size: size}))
		}
		if hasNext {
			row = append(row, p.ui.Button("Next ➡️", pluginkit.UIState{View: viewStatusList, Page: page + 1, Size: size}))
		}
		kb.Row(row...)
	}
	kb.Row(
		p.ui.Button("🔄 Refresh", pluginkit.UIState{View: viewStatusList, Page: page, Size: size}),
		p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}),
	)

	b.Inline(kb)
	return b.Build(), nil
}

func (p *Plugin) viewStatusUnit(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	mgr := p.mgrSnapshot()
	ctx2, cancel := p.opCtx(ctx)
	defer cancel()

	unit := normalizeUnit(st.Key)
	if unit == "" {
		return tgui.New().Title("⚠️", "systemd status").Line("missing unit name").Build(), nil
	}
	if !p.allowed(unit) {
		return tgui.New().Title("⚠️", "systemd status").Line("unit not allowed: " + unit).Build(), nil
	}
	if mgr == nil {
		kb := tgui.NewInline().Row(p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}))
		return tgui.New().Title("⚠️", "systemd status").Line("systemd manager not available").Inline(kb).Build(), nil
	}

	stt, err := mgr.GetStatusFullContext(ctx2, unit)
	if err != nil {
		kb := tgui.NewInline().Row(
			p.ui.Button("🔄 Refresh", pluginkit.UIState{View: viewStatusUnit, Key: unit}),
			p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}),
		)
		return tgui.New().Title("⚠️", "systemd status").Line("failed: " + err.Error()).Inline(kb).Build(), nil
	}

	// Compact, aligned, fixed-width details.
	b := tgui.New().Title(activeEmoji(stt.Active, stt.SubState), unit)
	b.Pre(detailTable(*stt))
	b.RawLine(hUpdated().String())

	kb := tgui.NewInline().
		Row(
			p.ui.Button("🔄 Refresh", pluginkit.UIState{View: viewStatusUnit, Key: unit}),
			p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}),
		).
		Row(
			p.ui.Button("▶️ Start", pluginkit.UIState{View: viewConfirmStart, Key: unit}),
			p.ui.Button("⏹ Stop", pluginkit.UIState{View: viewConfirmStop, Key: unit}),
			p.ui.Button("🔁 Restart", pluginkit.UIState{View: viewConfirmRestart, Key: unit}),
		).
		Row(
			p.ui.Button("⬅️ Back", pluginkit.UIState{View: viewStatusList, Page: 0, Size: 10}),
		)

	b.Inline(kb)
	return b.Build(), nil
}

// --- Operate flows (pick -> confirm -> exec) ---

func (p *Plugin) viewPickStart(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewPickOp(ctx, req, st, "start")
}
func (p *Plugin) viewPickStop(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewPickOp(ctx, req, st, "stop")
}
func (p *Plugin) viewPickRestart(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewPickOp(ctx, req, st, "restart")
}
func (p *Plugin) viewPickEnable(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewPickOp(ctx, req, st, "enable")
}
func (p *Plugin) viewPickDisable(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewPickOp(ctx, req, st, "disable")
}

func (p *Plugin) viewPickOp(ctx context.Context, req *core.Request, st pluginkit.UIState, action string) (tgui.Message, error) {
	cfg := p.cfgSnapshot()
	units := append([]string(nil), cfg.AllowUnits...)
	if len(units) == 0 {
		kb := tgui.NewInline().Row(p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}))
		return tgui.New().Title("⚠️", "systemd").Line("no managed units configured").Inline(kb).Build(), nil
	}
	sort.Strings(units)

	sub, page, size, _, _, hasPrev, hasNext := tgui.PaginateSlice(units, st.Page, st.Size)
	emoji, title, _ := opTitle(action)

	b := tgui.New().Title(emoji, title)
	// Compact header: action + page info (monospace).
	b.Code(action + "  " + pageShort(page, size, len(units)))

	cols := 2
	btns := make([]tele.Btn, 0, len(sub))
	confirmView := confirmViewForAction(action)
	for _, u := range sub {
		btns = append(btns, p.ui.Button(u, pluginkit.UIState{View: confirmView, Key: u}))
	}

	kb := tgui.NewInline()
	for i := 0; i < len(btns); i += cols {
		end := i + cols
		if end > len(btns) {
			end = len(btns)
		}
		kb.Row(btns[i:end]...)
	}
	if hasPrev || hasNext {
		row := make([]tele.Btn, 0, 2)
		if hasPrev {
			row = append(row, p.ui.Button("⬅️ Prev", pluginkit.UIState{View: pickViewForAction(action), Page: page - 1, Size: size}))
		}
		if hasNext {
			row = append(row, p.ui.Button("Next ➡️", pluginkit.UIState{View: pickViewForAction(action), Page: page + 1, Size: size}))
		}
		kb.Row(row...)
	}
	kb.Row(
		p.ui.Button("📄 Status", pluginkit.UIState{View: viewStatusList, Page: 0, Size: 10}),
		p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}),
	)

	b.Inline(kb)
	return b.Build(), nil
}

func (p *Plugin) viewConfirmStart(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewConfirmOp(ctx, req, st, "start")
}
func (p *Plugin) viewConfirmStop(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewConfirmOp(ctx, req, st, "stop")
}
func (p *Plugin) viewConfirmRestart(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewConfirmOp(ctx, req, st, "restart")
}
func (p *Plugin) viewConfirmEnable(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewConfirmOp(ctx, req, st, "enable")
}
func (p *Plugin) viewConfirmDisable(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewConfirmOp(ctx, req, st, "disable")
}

func (p *Plugin) viewConfirmOp(ctx context.Context, req *core.Request, st pluginkit.UIState, action string) (tgui.Message, error) {
	unit := normalizeUnit(st.Key)
	if unit == "" {
		return tgui.New().Title("⚠️", "systemd").Line("missing unit name").Build(), nil
	}
	if !p.allowed(unit) {
		return tgui.New().Title("⚠️", "systemd").Line("unit not allowed: " + unit).Build(), nil
	}
	emoji, title, verb := opTitle(action)
	if verb == "" {
		verb = action
	}
	b := tgui.New().Title(emoji, title)
	b.Pre(fmt.Sprintf("action: %s\nunit:   %s", verb, unit))
	b.RawLine(hUpdated().String())
	msg := b.Build()

	yes := p.ui.Button("✅ Yes", pluginkit.UIState{View: execViewForAction(action), Key: unit})
	cancel := p.ui.Button("↩️ Cancel", pluginkit.UIState{View: viewStatusUnit, Key: unit})
	list := p.ui.Button("📋 Unit list", pluginkit.UIState{View: pickViewForAction(action), Page: 0, Size: 12})
	close := p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed})

	kb := tgui.NewInline().Row(yes, cancel).Row(list, close)
	msg.Opt.ReplyMarkupAdapter = kb.Markup()
	return msg, nil
}

func (p *Plugin) viewExecStart(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewExecOp(ctx, req, st, "start")
}
func (p *Plugin) viewExecStop(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewExecOp(ctx, req, st, "stop")
}
func (p *Plugin) viewExecRestart(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewExecOp(ctx, req, st, "restart")
}
func (p *Plugin) viewExecEnable(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewExecOp(ctx, req, st, "enable")
}
func (p *Plugin) viewExecDisable(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	return p.viewExecOp(ctx, req, st, "disable")
}

func (p *Plugin) viewExecOp(ctx context.Context, req *core.Request, st pluginkit.UIState, action string) (tgui.Message, error) {
	unit := normalizeUnit(st.Key)
	if unit == "" {
		return tgui.New().Title("⚠️", "systemd").Line("missing unit name").Build(), nil
	}
	if !p.allowed(unit) {
		return tgui.New().Title("⚠️", "systemd").Line("unit not allowed: " + unit).Build(), nil
	}

	mgr := p.mgrSnapshot()
	if mgr == nil {
		kb := tgui.NewInline().Row(p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}))
		return tgui.New().Title("⚠️", "systemd").Line("systemd manager not available").Inline(kb).Build(), nil
	}

	ctx2, cancel := p.opCtx(ctx)
	defer cancel()
	started := time.Now()

	res, err := p.execSingle(ctx2, mgr, action, unit)
	results := []opRes{res}
	p.auditOperate(ctx2, req, action, []string{unit}, results, time.Since(started))

	emoji, title, _ := opTitle(action)
	b := tgui.New().Title(emoji, title)
	if err != nil {
		b.Code("FAIL " + action + " " + unit)
		b.Pre(err.Error())
	} else {
		b.Code("OK   " + action + " " + unit)
	}
	if res.Error != "" {
		// In some cases systemd returns multi-line diagnostics.
		b.PreMulti(res.Error)
	}
	b.RawLine(hUpdated().String())

	// Next actions
	kb := tgui.NewInline().
		Row(
			p.ui.Button("📄 Status", pluginkit.UIState{View: viewStatusUnit, Key: unit}),
			p.ui.Button("🔄 Refresh", pluginkit.UIState{View: viewStatusList, Page: 0, Size: 10}),
		).
		Row(
			p.ui.Button("🔁 Again", pluginkit.UIState{View: confirmViewForAction(action), Key: unit}),
			p.ui.Button("✖️ Close", pluginkit.UIState{View: viewClosed}),
		)

	b.Inline(kb)
	return b.Build(), nil
}

func (p *Plugin) execSingle(ctx context.Context, mgr *sm.ServiceManager, action, unit string) (opRes, error) {
	switch action {
	case "start":
		r := mgr.StartWithResult(ctx, unit)
		res := opRes{Unit: r.ServiceName, Success: r.Success, Error: errStr(r.Error)}
		return res, r.Error
	case "stop":
		r := mgr.StopWithResult(ctx, unit)
		res := opRes{Unit: r.ServiceName, Success: r.Success, Error: errStr(r.Error)}
		return res, r.Error
	case "restart":
		r := mgr.RestartWithResult(ctx, unit)
		res := opRes{Unit: r.ServiceName, Success: r.Success, Error: errStr(r.Error)}
		return res, r.Error
	case "enable":
		e := mgr.EnableContext(ctx, unit)
		res := opRes{Unit: unit, Success: e == nil, Error: errStr(e)}
		return res, e
	case "disable":
		e := mgr.DisableContext(ctx, unit)
		res := opRes{Unit: unit, Success: e == nil, Error: errStr(e)}
		return res, e
	default:
		return opRes{Unit: unit, Success: false, Error: "unknown action"}, errors.New("unknown action")
	}
}

func (p *Plugin) viewClosed(ctx context.Context, req *core.Request, st pluginkit.UIState) (tgui.Message, error) {
	b := tgui.New().Title("✖️", "Closed")
	b.RawLine(tgui.I("Tampilan ditutup.").String())
	return b.Build(), nil
}
