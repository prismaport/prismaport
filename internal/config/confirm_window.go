package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/tuning"
)

// ConfirmWindow is the dashboard's confirmation window (D59). Length
// (PRISMAPORT_CONFIRM_WINDOW, default 10m, at most 1h; 0 turns it off) is how long a right
// code lets the session's next covered Live actions run without another. Wallets
// (PRISMAPORT_CONFIRM_WINDOW_WALLETS, default 20) caps the Live wallets added in one window.
type ConfirmWindow struct {
	Length  time.Duration
	Wallets int
}

const maxConfirmWindow = time.Hour

func loadConfirmWindow() (ConfirmWindow, error) {
	w := ConfirmWindow{Length: tuning.DefaultConfirmWindow, Wallets: tuning.DefaultConfirmWindowWallets}
	var err error
	if w.Length, err = durationOr("PRISMAPORT_CONFIRM_WINDOW", w.Length); err != nil {
		return w, err
	}
	if w.Length < 0 || w.Length > maxConfirmWindow {
		return w, fmt.Errorf("PRISMAPORT_CONFIRM_WINDOW must be between 0 (off) and %s, got %s", maxConfirmWindow, w.Length)
	}
	if v := strings.TrimSpace(os.Getenv("PRISMAPORT_CONFIRM_WINDOW_WALLETS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return w, fmt.Errorf("PRISMAPORT_CONFIRM_WINDOW_WALLETS must be a whole number of at least 1, got %q", v)
		}
		w.Wallets = n
	}
	return w, nil
}
