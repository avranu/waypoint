// Command waypoint runs the Waypoint server and its admin commands.
//
//	waypoint                        serve (default)
//	waypoint invite [--admin] [email]  print a signup link
//	waypoint reset-password <user>  print a password reset link
//	waypoint backup <file>          write a consistent copy of the database
//	waypoint healthcheck             exit 0 if the local server answers /healthz
//	waypoint version
//
// Configuration comes from the environment; see docs/DEPLOY.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.jmann.me/claude/waypoint/internal/api"
	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/db"
	"git.jmann.me/claude/waypoint/internal/tracing"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool reads a true/false setting (1, true, yes, on), case-insensitively.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "waypoint:", err)
		os.Exit(1)
	}
}

func newLogger() *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(env("WAYPOINT_LOG_LEVEL", "info"))); err != nil {
		lvl = slog.LevelInfo // invalid levels fall back to info (zero value)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if env("WAYPOINT_LOG_FORMAT", "json") == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// open opens the database and the service; logs, if set, keeps the log
// records for `wp server log` (serve only).
func open(ctx context.Context, log *slog.Logger, logs *core.LogRing) (*core.Service, *db.DB, error) {
	path := env("WAYPOINT_DB", "data/waypoint.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, nil, err
	}
	d, err := db.Open(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	keep, err := strconv.Atoi(env("WAYPOINT_BACKUP_KEEP", "14"))
	if err != nil || keep < 1 {
		log.Warn("WAYPOINT_BACKUP_KEEP must be a positive number; keeping 14", "value", os.Getenv("WAYPOINT_BACKUP_KEEP"))
		keep = 14
	}
	go core.WarmLogin() // the dummy login hash, off the start path (WP-1398)
	svc := core.New(d, core.Config{
		BaseURL:       env("WAYPOINT_BASE_URL", "http://localhost:8080"),
		BackupDir:     backupDir(path),
		BackupKeep:    keep,
		TranscriptDir: transcriptDir(path),
		Egress:        core.EgressPolicy{BlockPrivate: envBool("WAYPOINT_EGRESS_BLOCK_PRIVATE")},

		MaxAttachmentMB: maxAttachmentMB(log),
		Logs:            logs,
		Version:         version,
		// Each assistant may set its own in Admin → Assistants.
		Assistant: core.AssistantDefaults{
			KeyHeader: env("WAYPOINT_ASSISTANT_KEY_HEADER", ""),
			AgentURL:  env("WAYPOINT_ASSISTANT_AGENT_URL", ""),
		},
		// The meaning service deploy/meaning runs (WP-1035); empty goes without.
		MeaningURL:   env("WAYPOINT_AI_MEANING_URL", ""),
		MeaningToken: os.Getenv("WAYPOINT_AI_MEANING_TOKEN"),
	}, log)
	return svc, d, nil
}

// maxAttachmentMB reads WAYPOINT_MAX_ATTACHMENT_MB, the size limit of one
// attached file (25 unless set).
func maxAttachmentMB(log *slog.Logger) int {
	v := env("WAYPOINT_MAX_ATTACHMENT_MB", "")
	if v == "" {
		return core.DefaultMaxAttachmentMB
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1024 {
		log.Warn("WAYPOINT_MAX_ATTACHMENT_MB must be a number from 1 to 1024; using the default", "value", v, "default", core.DefaultMaxAttachmentMB)
		return core.DefaultMaxAttachmentMB
	}
	return n
}

// cliDir holds the wp binaries served at /cli: WAYPOINT_CLI_DIR, or a cli
// folder next to this executable (where `make build` puts them).
func cliDir() string {
	if v := env("WAYPOINT_CLI_DIR", ""); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "cli")
}

// backupDir is where daily backups go: WAYPOINT_BACKUP_DIR, or a backups
// folder next to the database. "off" disables them.
func backupDir(dbPath string) string {
	switch v := env("WAYPOINT_BACKUP_DIR", ""); v {
	case "off", "none":
		return ""
	case "":
		return filepath.Join(filepath.Dir(dbPath), "backups")
	default:
		return v
	}
}

// logDir is where the server log's day files go: WAYPOINT_LOG_DIR, or a
// logs folder next to the database. "off" keeps the log in memory only.
func logDir(dbPath string) string {
	switch v := env("WAYPOINT_LOG_DIR", ""); v {
	case "off", "none":
		return ""
	case "":
		return filepath.Join(filepath.Dir(dbPath), "logs")
	default:
		return v
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	log := newLogger()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "version":
		fmt.Println(version)
		return nil
	case "healthcheck":
		return healthcheck(ctx)
	case "serve":
		return serve(ctx, log)
	case "invite":
		fs := flag.NewFlagSet("invite", flag.ExitOnError)
		admin := fs.Bool("admin", false, "make the new user an admin")
		// Accept the flag after the email too (flag stops at the first
		// positional argument).
		var rest []string
		for _, a := range args {
			if a == "--admin" || a == "-admin" {
				*admin = true
			} else {
				rest = append(rest, a)
			}
		}
		if err := fs.Parse(rest); err != nil {
			return err
		}
		svc, d, err := open(ctx, log, nil)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		link, err := svc.CreateInvite(ctx, core.SystemActor(), fs.Arg(0), *admin, 7*24*time.Hour)
		if err != nil {
			return err
		}
		fmt.Println(link.URL)
		return nil
	case "backup":
		if len(args) != 1 {
			return errors.New("usage: waypoint backup <file>")
		}
		_, d, err := open(ctx, log, nil)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		if err := d.Backup(ctx, args[0]); err != nil {
			return err
		}
		fmt.Println("wrote", args[0])
		return nil
	case "reset-password":
		if len(args) != 1 {
			return errors.New("usage: waypoint reset-password <username>")
		}
		svc, d, err := open(ctx, log, nil)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		users, err := svc.ListUsers(ctx, core.SystemActor())
		if err != nil {
			return err
		}
		for _, u := range users {
			if strings.EqualFold(u.Username, args[0]) || strings.EqualFold(u.Email, args[0]) {
				link, err := svc.CreateResetLink(ctx, core.SystemActor(), u.ID)
				if err != nil {
					return err
				}
				fmt.Println(link.URL)
				return nil
			}
		}
		return fmt.Errorf("no user %q", args[0])
	}
	return fmt.Errorf("unknown command %q (serve, invite, reset-password, backup, healthcheck, version)", cmd)
}

// healthcheck probes the local server, for container health checks
// (distroless images have no curl).
func healthcheck(ctx context.Context) error {
	addr := env("WAYPOINT_ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", res.Status)
	}
	return nil
}

func serve(ctx context.Context, log *slog.Logger) error {
	// Before anything else, so the service's outbound clients get traced.
	stopTracing, err := tracing.Setup(ctx, version, log)
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stopTracing(flush); err != nil {
			log.Warn("tracing shutdown", "err", err)
		}
	}()
	if tracing.Enabled() {
		log = slog.New(tracing.LogHandler(log.Handler()))
		log.Info("tracing on (OTLP over HTTP)")
	}
	// The recent log, secrets blanked, for `wp server log` (WP-375).
	logs := core.NewLogRing(core.DefaultLogRecords, core.DefaultLogAge)
	logs.AddEnvSecrets(os.Environ())
	// Kept on disk too, so a restart or a deploy doesn't lose it (WP-893).
	if dir := logDir(env("WAYPOINT_DB", "data/waypoint.db")); dir != "" {
		days, err := strconv.Atoi(env("WAYPOINT_LOG_DAYS", strconv.Itoa(core.DefaultLogDays)))
		if err != nil || days < 1 {
			log.Warn("WAYPOINT_LOG_DAYS must be a positive number; keeping the default", "value", os.Getenv("WAYPOINT_LOG_DAYS"), "days", core.DefaultLogDays)
			days = core.DefaultLogDays
		}
		if err := logs.Persist(dir, days, core.DefaultLogDirBytes); err != nil {
			log.Warn("log files off: the log is kept in memory only", "dir", dir, "err", err)
		} else {
			defer logs.Close() // last: flushes what the shutdown logged
		}
	}
	log = slog.New(logs.Handler(log.Handler()))
	slog.SetDefault(log)
	svc, d, err := open(ctx, log, logs)
	if err != nil {
		return err
	}
	// From here everything logs through the service, which counts each
	// warning and error by where it was logged (WP-ADR139).
	log = svc.Log()
	slog.SetDefault(log)
	defer func() { _ = d.Close() }()
	svc.LoadLogSecrets(ctx)
	svc.StartHub()
	// Pages from before codes (WP-D11) get theirs, adopting codes their
	// imported titles carry.
	if err := svc.AssignPageCodes(ctx); err != nil {
		log.Warn("page codes not assigned", "err", err)
	}
	// Likewise decision notes get their ADR codes (WP-113).
	if err := svc.AssignDesignCodes(ctx); err != nil {
		log.Warn("decision codes not assigned", "err", err)
	}
	if link, err := svc.BootstrapInvite(ctx); err != nil {
		return err
	} else if link != nil {
		// Printed, not logged: the link is a credential and logs get shipped
		// elsewhere. `waypoint invite --admin` makes a new one if it's lost.
		fmt.Fprintf(os.Stderr, "\n  Create your admin account: %s\n\n", link.URL)
		log.Warn("no users yet: open the admin link printed on stderr (valid 24h), or run `waypoint invite --admin`")
	}
	// An opencode server deployed alongside (see deploy/compose.yaml) becomes
	// the default AI provider until an admin configures something else.
	if u := env("WAYPOINT_AI_OPENCODE_URL", ""); u != "" {
		if err := svc.EnsureDefaultAI(ctx, u, os.Getenv("WAYPOINT_AI_OPENCODE_PASSWORD"), env("WAYPOINT_AI_MODEL", "opencode/big-pickle")); err != nil {
			log.Warn("default AI provider not set up", "err", err)
		}
	}
	// An LLM gateway, when given a key, goes first for every AI task; what
	// was set up before stays as its fallbacks. Without LLM_GATEWAY_URL it
	// keeps the URL saved by an earlier start.
	if g, notices, err := gatewayFromEnv(os.Getenv); err != nil {
		log.Warn("LLM gateway not set up", "err", err)
	} else if g != nil {
		for _, n := range notices {
			log.Warn(n)
		}
		if err := svc.EnsureLLMGateway(ctx, *g); err != nil {
			log.Warn("LLM gateway not set up", "err", err)
		}
	}
	// Pull request reviews start with the free models, Muse Spark first (WP-D14).
	if err := svc.EnsureReviewModels(ctx); err != nil {
		log.Warn("review models not set up", "err", err)
	}
	trusted, err := api.ParseTrustedProxies(os.Getenv("WAYPOINT_TRUST_PROXY"))
	if err != nil {
		log.Error("WAYPOINT_TRUST_PROXY", "err", err)
		os.Exit(1)
	}
	srv := api.New(svc, api.Config{
		CLIDir:         cliDir(),
		BaseURL:        svc.BaseURL(),
		TrustedProxies: trusted,
		MetricsToken:   env("WAYPOINT_METRICS_TOKEN", ""),
		DevProxy:       env("WAYPOINT_DEV_PROXY", ""),
		Version:        version,
	}, log)
	httpSrv := &http.Server{
		Addr:              env("WAYPOINT_ADDR", ":8080"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // bodies are small; this bounds slow uploads
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// No WriteTimeout: live updates (SSE) hold responses open.
	}
	httpSrv.RegisterOnShutdown(srv.Shutdown)
	go svc.RunScheduler(ctx)
	go svc.RunBots(ctx)
	go svc.RunAutomationRuns(ctx)
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpSrv.Addr, "base_url", svc.BaseURL(), "version", version)
		errc <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = httpSrv.Shutdown(shutdown)
	svc.Close()
	svc.Wait()
	return err
}

// transcriptDir is where the transcript archive's compressed pieces go:
// WAYPOINT_TRANSCRIPT_DIR, or a transcripts folder next to the database.
// "off" keeps no archive.
func transcriptDir(dbPath string) string {
	switch v := env("WAYPOINT_TRANSCRIPT_DIR", ""); v {
	case "off", "none":
		return ""
	case "":
		return filepath.Join(filepath.Dir(dbPath), "transcripts")
	default:
		return v
	}
}
