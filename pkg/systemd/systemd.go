package systemd

import (
	"context"
	"os/exec"
	"strings"
)

func IsActive(ctx context.Context, unit string) (bool, error) {
	cmd := exec.CommandContext(ctx, "systemctl", "is-active", unit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// is-active returns non-zero when inactive; treat as not active
		s := strings.TrimSpace(string(out))
		return s == "active", nil
	}
	return strings.TrimSpace(string(out)) == "active", nil
}

func Start(ctx context.Context, unit string) error {
	return exec.CommandContext(ctx, "systemctl", "start", unit).Run()
}
func Stop(ctx context.Context, unit string) error {
	return exec.CommandContext(ctx, "systemctl", "stop", unit).Run()
}
func Restart(ctx context.Context, unit string) error {
	return exec.CommandContext(ctx, "systemctl", "restart", unit).Run()
}
