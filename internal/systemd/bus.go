package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// ErrNoBus means there is no session bus to talk to: no graphical session, an SSH login, a
// container. Everything that needs one is skipped, never retried in a loop.
var ErrNoBus = errors.New("systemd: no session bus")

// sessionBusAddress finds the session bus without ever starting one. godbus's own default
// falls back to `dbus-launch`, which would spawn a bus daemon from inside a service.
func sessionBusAddress() string {
	if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" {
		return a
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		p := filepath.Join(dir, "bus")
		if _, err := os.Stat(p); err == nil {
			return "unix:path=" + p
		}
	}
	return ""
}

// SessionBus connects to the session bus, ErrNoBus when there is none.
func SessionBus() (*dbus.Conn, error) {
	addr := sessionBusAddress()
	if addr == "" {
		return nil, ErrNoBus
	}
	conn, err := dbus.Connect(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoBus, err)
	}
	return conn, nil
}

// callTimeout bounds every D-Bus call: none of them is worth hanging a command or the daemon's
// loop for.
const callTimeout = 5 * time.Second

// UnitName is the service the packages install.
const UnitName = "jiku-docs-sync.service"

// Unit is what the user manager says about a unit.
type Unit struct {
	// LoadState is "loaded", or "not-found" when no unit file exists.
	LoadState string
	// ActiveState is "active", "inactive", "failed", "activating", "deactivating"...
	ActiveState string
	SubState    string
	// UnitFileState is "enabled", "disabled", "static"...
	UnitFileState string
	// ExecMainStatus is the exit status of the last run, for a service that exited.
	ExecMainStatus int32
	// StatusText is the STATUS= the service last sent.
	StatusText string
}

const (
	sdDest    = "org.freedesktop.systemd1"
	sdPath    = dbus.ObjectPath("/org/freedesktop/systemd1")
	sdManager = "org.freedesktop.systemd1.Manager"
)

// UserUnit asks the user's systemd manager (over the session bus, where it is registered) about
// a unit. A unit that does not exist is not an error: LoadState says "not-found".
func UserUnit(ctx context.Context, name string) (Unit, error) {
	conn, err := SessionBus()
	if err != nil {
		return Unit{}, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	var path dbus.ObjectPath
	mgr := conn.Object(sdDest, sdPath)
	if err := mgr.CallWithContext(ctx, sdManager+".LoadUnit", 0, name).Store(&path); err != nil {
		return Unit{}, fmt.Errorf("systemd: loading %s: %w", name, err)
	}
	obj := conn.Object(sdDest, path)
	var u Unit
	str := func(iface, prop string) string {
		v, err := obj.GetProperty(iface + "." + prop)
		if err != nil {
			return ""
		}
		s, _ := v.Value().(string)
		return s
	}
	u.LoadState = str("org.freedesktop.systemd1.Unit", "LoadState")
	u.ActiveState = str("org.freedesktop.systemd1.Unit", "ActiveState")
	u.SubState = str("org.freedesktop.systemd1.Unit", "SubState")
	u.UnitFileState = str("org.freedesktop.systemd1.Unit", "UnitFileState")
	u.StatusText = str("org.freedesktop.systemd1.Service", "StatusText")
	if v, err := obj.GetProperty("org.freedesktop.systemd1.Service.ExecMainStatus"); err == nil {
		u.ExecMainStatus, _ = v.Value().(int32)
	}
	return u, nil
}

// RestartUserUnit restarts a unit of the user's manager and returns once the job is queued. A
// failed unit is reset first: one that hit its start limit refuses a plain restart.
func RestartUserUnit(ctx context.Context, name string, failed bool) error {
	conn, err := SessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	mgr := conn.Object(sdDest, sdPath)
	if failed {
		if err := mgr.CallWithContext(ctx, sdManager+".ResetFailedUnit", 0, name).Err; err != nil {
			return err
		}
	}
	var job dbus.ObjectPath
	return mgr.CallWithContext(ctx, sdManager+".RestartUnit", 0, name, "replace").Store(&job)
}

// Enabled reports whether the unit starts with the session.
func (u Unit) Enabled() bool {
	return u.UnitFileState == "enabled" || u.UnitFileState == "enabled-runtime"
}

// Describe renders a unit's state in a few words, in the language of the daemon's STATUS texts.
func (u Unit) Describe() string {
	if u.LoadState == "not-found" || u.LoadState == "" {
		return "no instalado"
	}
	enabled := ""
	switch u.UnitFileState {
	case "enabled", "enabled-runtime":
		enabled = ", habilitado"
	case "disabled":
		enabled = ", no habilitado"
	}
	switch u.ActiveState {
	case "active", "reloading":
		return "activo (systemd --user" + enabled + ")"
	case "activating":
		return "arrancando (systemd --user" + enabled + ")"
	case "deactivating":
		return "deteniéndose (systemd --user" + enabled + ")"
	case "failed":
		s := "fallido"
		if u.ExecMainStatus != 0 {
			s += fmt.Sprintf(", código %d", u.ExecMainStatus)
			switch u.ExecMainStatus {
			case 77:
				s += ": ejecutá `jiku login`"
			case 78:
				s += ": configuración inválida, ver el log"
			}
		}
		return s + " (systemd --user" + enabled + ")"
	}
	return strings.TrimSpace("inactivo (systemd --user" + enabled + ")")
}
