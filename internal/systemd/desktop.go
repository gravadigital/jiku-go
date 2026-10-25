package systemd

import (
	"context"

	"github.com/godbus/dbus/v5"
)

// Urgency is the freedesktop notification urgency hint.
type Urgency byte

const (
	UrgencyNormal   Urgency = 1
	UrgencyCritical Urgency = 2
)

// DesktopNotify shows a notification through org.freedesktop.Notifications, the interface
// GNOME and KDE both serve. ErrNoBus when there is no session bus.
func DesktopNotify(ctx context.Context, summary, body string, urgency Urgency) error {
	conn, err := SessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(urgency))}
	var id uint32
	return conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications").
		CallWithContext(ctx, "org.freedesktop.Notifications.Notify", 0,
			"jiku docs", uint32(0), "", summary, body, []string{}, hints, int32(-1)).Store(&id)
}
