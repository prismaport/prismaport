package config

import (
	"testing"
	"time"
)

func TestConfirmWindow(t *testing.T) {
	t.Setenv("PRISMAPORT_CONFIRM_WINDOW", "")
	t.Setenv("PRISMAPORT_CONFIRM_WINDOW_WALLETS", "")
	w, err := loadConfirmWindow()
	if err != nil || w.Length != 10*time.Minute || w.Wallets != 20 {
		t.Fatalf("default: %+v %v", w, err)
	}
	t.Setenv("PRISMAPORT_CONFIRM_WINDOW", "0")
	t.Setenv("PRISMAPORT_CONFIRM_WINDOW_WALLETS", "5")
	if w, err = loadConfirmWindow(); err != nil || w.Length != 0 || w.Wallets != 5 {
		t.Fatalf("off, 5 wallets: %+v %v", w, err)
	}
	for _, bad := range [][2]string{{"2h", "5"}, {"-1m", "5"}, {"soon", "5"}, {"5m", "0"}, {"5m", "many"}} {
		t.Setenv("PRISMAPORT_CONFIRM_WINDOW", bad[0])
		t.Setenv("PRISMAPORT_CONFIRM_WINDOW_WALLETS", bad[1])
		if _, err := loadConfirmWindow(); err == nil {
			t.Fatalf("%v: no error", bad)
		}
	}
}
