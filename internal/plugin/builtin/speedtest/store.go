package speedtest

import speedpkg "pewbot/pkg/speedtest"

func (p *Plugin) history() *speedpkg.HistoryStore {
	cfg := p.getConfig()
	if cfg.HistoryFile == "" {
		return nil
	}

	p.storeMu.Lock()
	defer p.storeMu.Unlock()
	if p.store == nil || p.store.Filename != cfg.HistoryFile {
		p.store = speedpkg.NewHistoryStore(cfg.HistoryFile)
	}
	return p.store
}
