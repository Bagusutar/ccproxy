//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func systemdQuoteArg(arg string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(arg) + `"`
}

func systemdUnit(exePath string) string {
	return fmt.Sprintf(`[Unit]
Description=ccproxy - Claude Code model router
After=network-online.target

[Service]
Type=simple
ExecStart=%s --daemon
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`, systemdQuoteArg(exePath))
}

// Linux/WSL 上用 systemd user unit，同样不需要 root。
func enableAutostartOS(exePath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unit := systemdUnit(exePath)
	if err := os.WriteFile(filepath.Join(dir, "ccproxy.service"), []byte(unit), 0o644); err != nil {
		return err
	}
	if out, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
	}
	if out, err := run("systemctl", "--user", "enable", "--now", "ccproxy.service"); err != nil {
		return fmt.Errorf("systemctl enable: %v: %s", err, out)
	}
	return nil
}

func disableAutostartOS() error {
	var errs []error
	if out, err := run("systemctl", "--user", "disable", "--now", "ccproxy.service"); err != nil {
		errs = append(errs, fmt.Errorf("systemctl disable: %w: %s", err, strings.TrimSpace(string(out))))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "ccproxy.service")
	if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove systemd unit: %w", err))
	}
	if out, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		errs = append(errs, fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out))))
	}
	return errors.Join(errs...)
}

func isAutostartEnabledOS() bool {
	out, err := run("systemctl", "--user", "is-enabled", "ccproxy.service")
	return autostartEnabledOutput(out, err)
}

func autostartEnabledOutput(out []byte, err error) bool {
	return err == nil && strings.TrimSpace(string(out)) == "enabled"
}
