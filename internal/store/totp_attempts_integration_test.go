//go:build integration

package store_test

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/store"
)

// Each attempt is counted before its code is checked, under a row lock: of many parallel
// attempts only those before the first wait get through, each with its own number.
func TestTakeTOTPAttemptInParallel(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	u, err := st.CreateDashboardUser(ctx, store.DashboardUser{ID: ids.New(ids.User), MerchantID: merchant(t, st), Email: "a@example.com",
		PasswordHash: "$argon2id$x"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	delay := func(attempt int) time.Duration {
		if attempt < 3 {
			return 0
		}
		return time.Minute
	}
	var mu sync.Mutex
	var attempts []int
	var waits []time.Duration
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, wait, err := st.TakeTOTPAttempt(ctx, u.ID, store.TOTPRunSignIn, delay)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
				return
			}
			if wait > 0 {
				waits = append(waits, wait)
				return
			}
			attempts = append(attempts, n)
		}()
	}
	wg.Wait()
	sort.Ints(attempts)
	if len(attempts) != 3 || attempts[0] != 1 || attempts[1] != 2 || attempts[2] != 3 || len(waits) != 5 {
		t.Fatalf("attempts %v, waits %v", attempts, waits)
	}
	for _, w := range waits {
		if w != time.Minute {
			t.Fatalf("wait %s, want the full minute on a fixed clock", w)
		}
	}

	// The step-up run is apart: wrong codes before a session never make a signed-in user wait.
	if n, wait, err := st.TakeTOTPAttempt(ctx, u.ID, store.TOTPRunStepUp, delay); err != nil || wait != 0 || n != 1 {
		t.Fatalf("step-up run during the sign-in wait: attempt %d, wait %s, %v", n, wait, err)
	}
	if err := st.EndTOTPFailures(ctx, u.ID, store.TOTPRunStepUp); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UseTOTPStep(ctx, u.ID, store.TOTPRunSignIn, 1); err != nil || ok {
		t.Fatalf("a step accepted without an active secret: %v %v", ok, err)
	}
	var failures int
	if err := st.Pool().QueryRow(ctx, `SELECT totp_failures FROM dashboard_users WHERE id = $1`, u.ID).Scan(&failures); err != nil || failures != 3 {
		t.Fatalf("failures %d: %v", failures, err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE dashboard_users SET totp_secret_ciphertext = '\x01' WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.UseTOTPStep(ctx, u.ID, store.TOTPRunSignIn, 1); err != nil || !ok {
		t.Fatalf("use step: %v %v", ok, err)
	}
	n, wait, err := st.TakeTOTPAttempt(ctx, u.ID, store.TOTPRunSignIn, delay)
	if err != nil || wait != 0 || n != 1 {
		t.Fatalf("after an accepted code: attempt %d, wait %s, %v", n, wait, err)
	}
}

// A recovery code can be spent by only one of many parallel requests; a code never issued is not
// spent, and a spent one stays spent.
func TestUseRecoveryCodeOnce(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	u, err := st.CreateDashboardUser(ctx, store.DashboardUser{ID: ids.New(ids.User), MerchantID: merchant(t, st), Email: "a@example.com",
		PasswordHash: "$argon2id$x"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	secret, session := []byte("sealed"), [32]byte{1}
	if err := st.SetTOTPPending(ctx, u.ID, secret, session, st.Now().Add(time.Minute), nil, "test"); err != nil {
		t.Fatal(err)
	}
	code := make([]byte, 32)
	code[0] = 7
	other := make([]byte, 32)
	other[0] = 8
	if err := st.EnableTOTP(ctx, u.ID, secret, session, nil, 1, [][]byte{code, other}, "test"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	used := 0
	for range 20 {
		wg.Go(func() {
			ok, err := st.UseRecoveryCode(ctx, u, store.TOTPRunSignIn, code, "test")
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				used++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if used != 1 {
		t.Fatalf("spent %d times, want once", used)
	}
	if ok, err := st.UseRecoveryCode(ctx, u, store.TOTPRunSignIn, make([]byte, 32), "test"); err != nil || ok {
		t.Fatalf("a code never issued: %v %v", ok, err)
	}
	fresh, err := st.DashboardUser(ctx, u.ID)
	if err != nil || fresh.RecoveryCodesLeft != 1 {
		t.Fatalf("left %d: %v", fresh.RecoveryCodesLeft, err)
	}
}

// Wrong passwords count in a row and a right one ends the run.
func TestPasswordFailures(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	u, err := st.CreateDashboardUser(ctx, store.DashboardUser{ID: ids.New(ids.User), MerchantID: merchant(t, st), Email: "p@example.com",
		PasswordHash: "$argon2id$x"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 3; want++ {
		if n, err := st.RecordPasswordFailure(ctx, u.ID); err != nil || n != want {
			t.Fatalf("failure %d: %d %v", want, n, err)
		}
	}
	if err := st.EndPasswordFailures(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RecordPasswordFailure(ctx, u.ID); err != nil || n != 1 {
		t.Fatalf("after a right password: %d %v", n, err)
	}
}

// A lost link takes maxTries wrong passwords and is then deleted; a new link starts over.
func TestFailTOTPLostLink(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	u, err := st.CreateDashboardUser(ctx, store.DashboardUser{ID: ids.New(ids.User), MerchantID: merchant(t, st), Email: "l@example.com",
		PasswordHash: "$argon2id$x"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	link := make([]byte, 32)
	link[0] = 9
	if err := st.IssueTOTPLostLink(ctx, u, link, st.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for try := 1; try <= 3; try++ {
		ended, err := st.FailTOTPLostLink(ctx, link, 3)
		if err != nil || ended != (try == 3) {
			t.Fatalf("try %d: ended %v, %v", try, ended, err)
		}
	}
	var left int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM dashboard_totp_lost_links WHERE user_id = $1`, u.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("links left %d: %v", left, err)
	}
	if ended, err := st.FailTOTPLostLink(ctx, link, 3); err != nil || ended {
		t.Fatalf("a deleted link: ended %v, %v", ended, err)
	}
	if err := st.IssueTOTPLostLink(ctx, u, link, st.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if ended, err := st.FailTOTPLostLink(ctx, link, 3); err != nil || ended {
			t.Fatalf("a new link: ended %v, %v", ended, err)
		}
	}
	// Replacing a live link (the ON CONFLICT path) starts its count over.
	next := make([]byte, 32)
	next[0] = 10
	if err := st.IssueTOTPLostLink(ctx, u, next, st.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if ended, err := st.FailTOTPLostLink(ctx, next, 3); err != nil || ended {
			t.Fatalf("a replaced link: ended %v, %v", ended, err)
		}
	}
}

// A removal is never carried out before its time, and at its time only once.
func TestRemoveDueTOTPNotEarly(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	ctx := t.Context()
	u, err := st.CreateDashboardUser(ctx, store.DashboardUser{ID: ids.New(ids.User), MerchantID: merchant(t, st), Email: "b@example.com",
		PasswordHash: "$argon2id$x"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	now := st.Now()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE dashboard_users SET totp_secret_ciphertext = 'x' WHERE id = $1`, u.ID)
	exec(`INSERT INTO dashboard_totp_removals (user_id, requested_at, removal_at, cancel_token_hash) VALUES ($1, $2, $3, $4)`, u.ID, now.Add(-time.Hour), now.Add(time.Hour), make([]byte, 32))
	if _, found, err := st.RemoveDueTOTP(ctx, 48*time.Hour); err != nil || found {
		t.Fatalf("removed early: %v %v", found, err)
	}
	exec(`UPDATE dashboard_totp_removals SET removal_at = $2 WHERE user_id = $1`, u.ID, now.Add(-time.Second))
	got, found, err := st.RemoveDueTOTP(ctx, 48*time.Hour)
	if err != nil || !found || got.UserID != u.ID || !got.LockedUntil.Equal(now.Add(-time.Second).Add(48*time.Hour)) {
		t.Fatalf("%+v %v %v", got, found, err)
	}
	if _, found, err := st.RemoveDueTOTP(ctx, 48*time.Hour); err != nil || found {
		t.Fatalf("removed twice: %v %v", found, err)
	}
}
