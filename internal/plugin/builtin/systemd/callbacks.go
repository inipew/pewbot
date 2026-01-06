package systemd

import core "pewbot/internal/plugin"

func (p *Plugin) Callbacks() []core.CallbackRoute {
	if p.ui == nil {
		return nil
	}
	return []core.CallbackRoute{p.ui.Route()}
}
