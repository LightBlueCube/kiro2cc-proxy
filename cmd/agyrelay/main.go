// Command agyrelay serves an OpenAI-compatible API backed by Antigravity
// accounts through `agy acp`, with an admin UI for accounts, keys, usage,
// and logs.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"agyrelay/internal/admin"
	"agyrelay/internal/agybin"
	"agyrelay/internal/bridge"
	"agyrelay/internal/cloudcode"
	"agyrelay/internal/control"
	"agyrelay/internal/egress"
	"agyrelay/internal/gateway"
	"agyrelay/internal/logbuf"
	"agyrelay/internal/openai"
	"agyrelay/internal/pool"
	"agyrelay/internal/server"
	"agyrelay/internal/store"
	"agyrelay/internal/usage"
)

// logBufferEntries is how many log entries the admin UI can show.
const logBufferEntries = 10000

const commandUsage = `usage:
  agyrelay serve  -config FILE   run the API server and admin UI
  agyrelay passwd -config FILE   set the admin password (signs every admin out)
  agyrelay check-agy BIN         report whether an agy executable is pinned and patchable`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, commandUsage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "passwd":
		err = runPasswd(os.Args[2:])
	case "check-agy":
		err = runCheckAgy(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, commandUsage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agyrelay:", err)
		os.Exit(1)
	}
}

func parseConfigFlag(name string, args []string) (*config, error) {
	flags := flag.NewFlagSet(name, flag.ExitOnError)
	path := flags.String("config", "agyrelay.json", "configuration file")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return loadConfig(*path)
}

func openStore(cfg *config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	return store.Open(cfg.dbPath())
}

func runServe(args []string) error {
	cfg, err := parseConfigFlag("serve", args)
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	settings, err := control.LoadSettings(ctx, st)
	if err != nil {
		return err
	}
	level := new(slog.LevelVar)
	level.Set(settings.Level())
	logs := logbuf.NewBuffer(logBufferEntries)
	stderr := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}) // level gates in logbuf
	log := slog.New(logbuf.NewHandler(logs, stderr, level))

	agyBin, err := deriveAgy(cfg, log)
	if err != nil {
		return err
	}

	tools, err := bridge.Start(log.With("component", "bridge"))
	if err != nil {
		return err
	}
	defer tools.Close()

	accountPool, err := pool.New(pool.Config{
		Bin:               agyBin,
		TurnsPerAccount:   settings.TurnsPerAccount,
		ProcessPerSession: settings.PerSession(),
		Log:               log.With("component", "pool"),
	})
	if err != nil {
		return err
	}
	defer accountPool.Close()

	usageLog := log.With("component", "usage")
	recorder := usage.NewRecorder(st, settings.Prices, usageLog)
	budget := usage.NewBudget(st)
	outbound, err := egress.Start(log.With("component", "egress"))
	if err != nil {
		return err
	}
	defer outbound.Close()
	cloudCode, err := cloudcode.Start(cloudcode.DefaultUpstream, log.With("component", "cloudcode"))
	if err != nil {
		return err
	}
	defer cloudCode.Close()
	gw, err := gateway.New(gateway.Config{
		Pool:        accountPool,
		Bridge:      tools,
		DataDir:     cfg.DataDir,
		SnapshotTTL: settings.SnapshotTTLDuration(),
		IdleClose:   settings.SessionIdleCloseDuration(),
		Recorder:    recorder,
		Budget:      budget,
		Log:         log.With("component", "gateway"),
	})
	if err != nil {
		return err
	}
	defer gw.Close()

	monitor := usage.NewMonitor(usage.MonitorConfig{
		Store: st, Pool: accountPool, Profile: gateway.IdleProfile(),
		OnSample: usage.NewAttributor(st, usageLog).Attribute, Log: usageLog,
	})
	go monitor.Run(ctx)

	ctl, err := control.New(ctx, control.Config{
		Store: st, Pool: accountPool, Gateway: gw, Monitor: monitor, Recorder: recorder, Budget: budget, Egress: outbound, CloudCode: cloudCode, AgyBin: agyBin, Level: level,
		AccountsDir: cfg.accountsDir(), Settings: settings, Log: log.With("component", "control"),
	})
	if err != nil {
		return err
	}
	go ctl.RunRetention(ctx)

	var adminUI http.Handler
	if cfg.loopbackOnly() {
		adminUI = admin.New(admin.Config{
			Store: st, Control: ctl, Pool: accountPool, Monitor: monitor, Logs: logs, Log: log.With("component", "admin"),
		})
	} else {
		log.Warn("admin UI disabled: it needs a loopback listen address behind a TLS tunnel or proxy", "listen", cfg.Listen)
	}

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(server.Config{Keys: ctl, OpenAI: openai.NewHandler(gw), Admin: adminUI}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- httpServer.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "accounts", len(accountPool.Accounts()))

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// deriveAgy returns the patched agy to run. The relay never runs an
// unpatched agy: any failed check stops startup.
func deriveAgy(cfg *config, log *slog.Logger) (string, error) {
	path, report, err := agybin.Derive(cfg.AgyBin, cfg.derivedAgyDir())
	if report != nil {
		for _, s := range report.Sites {
			if s.Err != nil {
				log.Error("agy patch failed", "patch", s.Patch, "function", s.Function, "matches", s.Matches,
					"agy_version", report.Version(), "agy_sha256", report.SHA256, "err", s.Err)
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("refusing to start with agy %s: %w", cfg.AgyBin, err)
	}
	// The check held the whole executable (~250 MB) in memory.
	debug.FreeOSMemory()
	log.Info("agy patched", "agy_version", report.Version(), "agy_sha256", report.SHA256, "derived", path)
	return path, nil
}

// runCheckAgy reports, for an agy upgrade, whether each patch site still
// matches exactly once. It exits non-zero unless the build would start.
func runCheckAgy(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agyrelay check-agy BIN")
	}
	report, err := agybin.Check(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("sha256  %s\nversion %s\n", report.SHA256, report.Version())
	for _, s := range report.Sites {
		status := "ok"
		if s.Err != nil {
			status = s.Err.Error()
		}
		fmt.Printf("%-18s %-3d %s\n    %s\n", s.Patch, s.Matches, status, s.Function)
	}
	return report.Err()
}

// runPasswd reads the new password twice: hidden from a terminal, or as
// two lines from a pipe.
func runPasswd(args []string) error {
	cfg, err := parseConfigFlag("passwd", args)
	if err != nil {
		return err
	}
	read := readPasswordLine()
	first, err := read("New admin password: ")
	if err != nil {
		return err
	}
	if err := admin.ValidatePassword(first); err != nil {
		return err
	}
	second, err := read("Again: ")
	if err != nil {
		return err
	}
	if first != second {
		return errors.New("the passwords differ")
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := admin.SetPassword(context.Background(), st, first); err != nil {
		return err
	}
	fmt.Println("Admin password set; every admin session was signed out.")
	return nil
}

func readPasswordLine() func(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		return func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			raw, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			return string(raw), err
		}
	}
	lines := bufio.NewScanner(os.Stdin)
	return func(string) (string, error) {
		if !lines.Scan() {
			if err := lines.Err(); err != nil {
				return "", err
			}
			return "", errors.New("expected two password lines on stdin")
		}
		return strings.TrimRight(lines.Text(), "\r"), nil
	}
}
