// Command prismaport is the single backend binary: api, worker, migrate, the Pages build and the
// test-mode developer tools (listen, trigger).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prismaport/prismaport/internal/api"
	"github.com/prismaport/prismaport/internal/app"
	"github.com/prismaport/prismaport/internal/config"
	"github.com/prismaport/prismaport/internal/db"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/version"
	"github.com/prismaport/prismaport/internal/webhook"
	"github.com/prismaport/prismaport/internal/worker"
	checkoutweb "github.com/prismaport/prismaport/web/checkout"
	dashboardweb "github.com/prismaport/prismaport/web/dashboard"
	docsweb "github.com/prismaport/prismaport/web/docs"
	landingweb "github.com/prismaport/prismaport/web/landing"
)

const usage = `usage: prismaport <command> [flags]

commands:
  api                         serve the HTTP API
  worker [chain|events]       run every background loop, or only the chain or the events part
  migrate up|down|status      manage the database schema
  version                     print the release, the commit and the build time
  pages build --out DIR --site-url URL --app-url URL --pay-url URL --api-url URL
                              write the Cloudflare Pages directories (site/, app/, pay/)
                              of the split frontend from the pages in this binary

test mode (an sk_test_ key; never live):
  listen --api-key sk_test_... --forward-to URL [--events a,b] [--api-url URL] [--after N]
                              forward new events to a local URL, signed with a secret
                              printed at start
  trigger <event> --api-key sk_test_... [--api-url URL] [--settle]
                              create a test session and simulate it into that event;
                              --settle then settles it and prints the settlement
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return errors.New("missing command")
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	switch args[0] {
	case "api":
		return runAPI(ctx, log)
	case "worker":
		return runWorker(ctx, args[1:], log)
	case "migrate":
		return runMigrate(ctx, args[1:], out, log)
	case "listen":
		return runListen(ctx, args[1:], out)
	case "trigger":
		return runTrigger(ctx, args[1:], out)
	case "pages":
		return runPages(args[1:], out, embeddedPages())
	case "version", "--version":
		i := version.Get()
		fmt.Fprintf(out, "prismaport %s (commit %s", i.Version, i.Commit)
		if i.BuiltAt != "" {
			fmt.Fprintf(out, ", built %s", i.BuiltAt)
		}
		fmt.Fprintln(out, ")")
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return nil
	}
	fmt.Fprint(out, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

// open is the API's pool: every statement runs under db.AppTimeouts.
func open(ctx context.Context, log *slog.Logger) (config.Env, *pgxpool.Pool, error) {
	return openDB(ctx, log, false, db.AppTimeouts)
}

// openSession is open for the processes that rely on session state: the worker's chain locks
// and the migrations.
func openSession(ctx context.Context, log *slog.Logger, t db.Timeouts) (config.Env, *pgxpool.Pool, error) {
	return openDB(ctx, log, true, t)
}

// openDB opens the pool, waiting for a free connection (waitForConn) when the database has none.
func openDB(ctx context.Context, log *slog.Logger, session bool, t db.Timeouts) (config.Env, *pgxpool.Pool, error) {
	env, err := config.Load()
	if err != nil {
		return env, nil, err
	}
	if session {
		if err := db.RequireSessionConnection(env.DatabaseURL); err != nil {
			return env, nil, err
		}
	}
	var pool *pgxpool.Pool
	err = waitForConn(ctx, log, "open", func() error {
		pool, err = db.OpenWith(ctx, env.DatabaseURL, t)
		return err
	})
	return env, pool, err
}

// connWait bounds how long a process keeps asking for a connection while the database turns it
// away for want of slots, and connRetry is the pause between tries. The Supabase session pooler
// has 15 slots and api + worker use 12 of them (pools of 4 and 6, the worker's lock and wake
// connections); during a deploy the old and new processes overlap for a few seconds and need more.
// Waiting rides that out; the slots free up once the old processes stop.
var (
	connWait  = 5 * time.Minute
	connRetry = 5 * time.Second
)

// waitForConn runs fn, and runs it again every connRetry while it fails with the connection limit
// (db.IsConnLimit), until connWait passes. fn must be safe to run again after such a failure.
func waitForConn(ctx context.Context, log *slog.Logger, what string, fn func() error) error {
	deadline := time.Now().Add(connWait)
	for {
		err := fn()
		if err == nil || !db.IsConnLimit(err) || time.Now().Add(connRetry).After(deadline) {
			return err
		}
		log.Warn(what+": database has no free connection, retrying", "in", connRetry, "err", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(connRetry):
		}
	}
}

// buildService builds the service and the store it runs over; the callers pass the store to
// what reads or writes the database next to the service.
func buildService(root app.Root, pool *pgxpool.Pool, needSecrets bool) (*service.Service, *store.Store, error) {
	st := root.Store(pool)
	svc, err := root.Service(st, needSecrets)
	return svc, st, err
}

// mountPages serves the web apps. The landing page's canonical and share-card URLs follow the
// site URL only: until it is configured they keep naming prismaport.com, never the Railway or
// localhost origin the api happens to answer on.
func mountPages(handler *api.Server, env config.Env) error {
	if err := handler.MountCheckoutPages(checkoutweb.Dist()); err != nil {
		return err
	}
	if err := handler.MountDashboardPages(dashboardweb.Dist()); err != nil {
		return err
	}
	handler.MountDocsPages(docsweb.Dist())
	handler.MountLandingPages(landingweb.Site(), env.SiteURL)
	return nil
}

// configureHosts sets split mode (PRISMAPORT_FRONTEND=external) or
// routing by host (PRISMAPORT_SITE_URL), or neither.
func configureHosts(handler *api.Server, env config.Env, log *slog.Logger) error {
	h := api.Hosts{Site: env.SiteURL, Dashboard: env.DashboardURL, Checkout: env.CheckoutBaseURL, API: env.APIURL, Docs: env.DocsURL, External: env.ExternalFrontend}
	if err := handler.ConfigureHosts(h); err != nil {
		if h.External {
			return fmt.Errorf("split frontend (PRISMAPORT_FRONTEND=external): %w", err)
		}
		return fmt.Errorf("host routing (PRISMAPORT_SITE_URL, PRISMAPORT_DASHBOARD_URL, PRISMAPORT_CHECKOUT_BASE_URL, PRISMAPORT_API_URL): %w", err)
	}
	switch {
	case h.External:
		log.Info("pages served elsewhere; serving the API only", "site", env.SiteURL, "dashboard", env.DashboardURL, "checkout", env.CheckoutBaseURL, "docs", env.DocsURL, "api", env.APIURL)
	case env.SiteURL != "":
		log.Info("routing by host", "site", env.SiteURL, "dashboard", env.DashboardURL, "checkout", env.CheckoutBaseURL, "api", env.APIURL)
	}
	return nil
}

// mailDrain bounds how long the API waits on shutdown for queued mail, after the HTTP server's
// own 10 seconds: both fit a platform grace period of 30 seconds.
const mailDrain = 10 * time.Second

func runAPI(ctx context.Context, log *slog.Logger) error {
	env, pool, err := open(ctx, log)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := env.CheckMail(); err != nil {
		return err
	}
	root := app.Root{Env: env, Log: log}
	mailer, stopMail, err := root.Mailer(log)
	if err != nil {
		return err
	}
	defer func() {
		// After the HTTP server shut down: what the requests queued gets until mailDrain to go.
		ctx, cancel := context.WithTimeout(context.Background(), mailDrain)
		defer cancel()
		if err := stopMail(ctx); err != nil {
			log.Warn("mail queue not drained before shutdown; the rest was dropped", "err", err)
		}
	}()
	if mailer == nil {
		log.Warn("self-serve sign-up is off: no mail transport is configured (PRISMAPORT_RESEND_API_KEY or PRISMAPORT_SMTP_HOST)")
	}
	root.Mail = mailer
	svc, st, err := buildService(root, pool, true)
	if err != nil {
		return err
	}
	handler := api.New(svc, st, log)
	defer handler.Close() // before the pool closes (deferred earlier, so run later)
	if err := handler.ConfigurePublic(api.PublicOptions{ClientIPHeader: os.Getenv("PRISMAPORT_CLIENT_IP_HEADER")}); err != nil {
		return err
	}
	if err := configureHosts(handler, env, log); err != nil {
		return err
	}
	alerts, deliverAlerts := root.Alerts(log)
	go deliverAlerts(ctx)
	handler.MountRoutes(root.Checkout(svc, st).WithAlerts(alerts), webhook.NewDispatcher(svc, st, webhook.Config{AllowPrivateNetworks: env.AllowPrivateWebhooks}, log))
	if !env.ExternalFrontend {
		if err := mountPages(handler, env); err != nil {
			return err
		}
	}
	srv := &http.Server{
		Addr:              env.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", env.HTTPAddr, "version", version.Label())
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// workerRole reads `prismaport worker [chain|events]`; with no argument the process runs every loop.
func workerRole(args []string) (worker.Role, error) {
	if len(args) == 0 {
		return worker.RoleAll, nil
	}
	if role := worker.Role(args[0]); len(args) == 1 && (role == worker.RoleChain || role == worker.RoleEvents) {
		return role, nil
	}
	return "", errors.New("usage: prismaport worker [chain|events]")
}

func runWorker(ctx context.Context, args []string, log *slog.Logger) error {
	role, err := workerRole(args)
	if err != nil {
		return err
	}
	env, pool, err := openSession(ctx, log, db.AppTimeouts)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.RequireMaxConns(pool.Config(), db.MinWorkerConns); err != nil {
		return err
	}
	// One idle/active decision for the whole process, marked by its loops, the chain scheduler and
	// the wake listener (plan B).
	act := worker.NewActivity(env.Worker.IdleAfter, nil, log)
	root := app.Root{Env: env, Log: log, Activity: act}
	if role != worker.RoleChain {
		// The emails about failing webhook endpoints wait for the provider's answer (a warning counts
		// only once it is accepted), so the worker sends through the transport, not the queue.
		transport, err := root.MailTransport()
		if err != nil {
			return err
		}
		if transport == nil {
			log.Warn("failing webhook endpoints are never warned by email, so never disabled: no mail transport is configured (PRISMAPORT_RESEND_API_KEY or PRISMAPORT_SMTP_HOST)")
		}
		root.Mail = transport
	}
	svc, st, err := buildService(root, pool, true)
	if err != nil {
		return err
	}
	alerts, deliverAlerts := root.Alerts(log)
	go deliverAlerts(ctx)
	var d *webhook.Dispatcher
	if role != worker.RoleChain {
		d = webhook.NewDispatcher(svc, st, webhook.Config{AllowPrivateNetworks: env.AllowPrivateWebhooks, Alerts: alerts}, log)
	}
	// The events role never builds the chain layer, so it starts without the gas wallets.
	var loops []worker.Loop
	if role != worker.RoleEvents {
		locks, err := store.NewLocker(env.DatabaseURL)
		if err != nil {
			return err
		}
		// Runs after the loops have stopped, so the locks are released on shutdown.
		defer locks.Close()
		if loops, err = root.ChainLoops(svc, st, locks, alerts, log); err != nil {
			return err
		}
	}
	log.Info("worker started", "role", role, "version", version.Label(), "chain_loops", len(loops), "alert_webhook", env.AlertWebhookURL != "")
	cfg := env.Worker.Config()
	cfg.Role, cfg.Loops, cfg.Alerts, cfg.IdempotencyTTL, cfg.StepTimeout, cfg.Activity = role, loops, alerts, env.IdempotencyTTL, env.StepTimeout, act
	// The listener holds one connection of the pool (db.MinWorkerConns counts it).
	cfg.Listen = func(ctx context.Context, wake func(string)) { st.ListenWake(ctx, env.Worker.Wake(), wake, log) }
	worker.New(svc, st, d, cfg, log).Run(ctx)
	return nil
}

// runMigrate applies, reverts or reports the migrations, logging each step; `down` never goes
// below the baseline. Migrate takes two connections (the lock's and the pool's), so the pre-deploy
// run can open its pool and still be turned away by the connection limit on the second: an `up`
// or `status` then starts over (waitForConn). The failed run closes its lock connection, and the
// migrations already applied are skipped. `down` never starts over, since a retry after a revert
// would revert one more.
func runMigrate(ctx context.Context, args []string, out io.Writer, log *slog.Logger) error {
	if len(args) != 1 {
		return errors.New("usage: prismaport migrate up|down|status")
	}
	if args[0] == "down" {
		return migrateOnce(ctx, args[0], out, log)
	}
	return waitForConn(ctx, log, "migrate", func() error { return migrateOnce(ctx, args[0], out, log) })
}

func migrateOnce(ctx context.Context, action string, out io.Writer, log *slog.Logger) error {
	_, pool, err := openSession(ctx, log, db.Timeouts{}) // a migration may run past the app's statement timeout
	if err != nil {
		return err
	}
	defer pool.Close()
	m, err := db.NewMigrator(pool)
	if err != nil {
		return err
	}
	defer m.Close()
	m.SetLogger(log)
	switch action {
	case "up":
		if err := m.Up(ctx); err != nil {
			return err
		}
	case "down":
		v, err := m.Version(ctx)
		if err != nil {
			return err
		}
		prev := int64(0)
		for _, x := range m.Versions() {
			if x < v {
				prev = x
			}
		}
		if err := m.DownTo(ctx, prev); err != nil {
			return err
		}
	case "status":
		st, err := m.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "schema version %d\nstate: %s\n", st.Version, st)
		return nil
	default:
		return fmt.Errorf("unknown migrate action %q", action)
	}
	v, err := m.Version(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "schema version %d\n", v)
	return nil
}
