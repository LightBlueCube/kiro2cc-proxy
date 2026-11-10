// Command agyrelay serves an OpenAI-compatible API backed by Antigravity
// accounts through `agy acp`.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"agyrelay/internal/bridge"
	"agyrelay/internal/gateway"
	"agyrelay/internal/openai"
	"agyrelay/internal/pool"
	"agyrelay/internal/server"
)

const usage = `usage:
  agyrelay serve  -config FILE          run the API server
  agyrelay login  -config FILE ACCOUNT  sign an account in (interactive agy)
  agyrelay keygen NAME                  create an API key and its config entry`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "login":
		err = runLogin(os.Args[2:])
	case "keygen":
		err = runKeygen(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agyrelay:", err)
		os.Exit(1)
	}
}

func parseConfigFlag(name string, args []string) (*config, []string, error) {
	flags := flag.NewFlagSet(name, flag.ExitOnError)
	path := flags.String("config", "agyrelay.json", "configuration file")
	if err := flags.Parse(args); err != nil {
		return nil, nil, err
	}
	cfg, err := loadConfig(*path)
	return cfg, flags.Args(), err
}

func runServe(args []string) error {
	cfg, _, err := parseConfigFlag("serve", args)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}

	tools, err := bridge.Start(log.With("component", "bridge"))
	if err != nil {
		return err
	}
	defer tools.Close()

	accounts := make([]pool.AccountConfig, len(cfg.Accounts))
	for i, a := range cfg.Accounts {
		accounts[i] = pool.AccountConfig{Name: a.Name, Home: cfg.accountHome(a.Name), Env: a.Env}
	}
	accountPool, err := pool.New(pool.Config{
		Bin:               cfg.AgyBin,
		Accounts:          accounts,
		TurnsPerAccount:   cfg.TurnsPerAccount,
		ProcessPerSession: *cfg.ProcessPerSession,
		Log:               log.With("component", "pool"),
	})
	if err != nil {
		return err
	}
	defer accountPool.Close()

	gw, err := gateway.New(gateway.Config{
		Pool:        accountPool,
		Bridge:      tools,
		DataDir:     cfg.DataDir,
		SnapshotTTL: cfg.snapshotTTL,
		Log:         log.With("component", "gateway"),
	})
	if err != nil {
		return err
	}
	defer gw.Close()

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(cfg.keys, openai.NewHandler(gw)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- httpServer.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "accounts", len(accounts))

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

// runLogin starts agy's interactive UI with the account's HOME, where agy
// stores its sign-in.
func runLogin(args []string) error {
	cfg, rest, err := parseConfigFlag("login", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("login needs exactly one account name")
	}
	name := rest[0]
	configured := false
	for _, a := range cfg.Accounts {
		configured = configured || a.Name == name
	}
	if !configured {
		return fmt.Errorf("account %q is not in the config", name)
	}
	home := cfg.accountHome(name)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	fmt.Printf("Starting agy with HOME=%s. Sign in, then exit agy.\n", home)
	cmd := exec.Command(cfg.AgyBin)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home, "AGY_CLI_DISABLE_AUTO_UPDATE=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func runKeygen(args []string) error {
	if len(args) != 1 {
		return errors.New("keygen needs exactly one key name")
	}
	key := "sk-agyrelay-" + rand.Text()
	sum := sha256.Sum256([]byte(key))
	fmt.Printf("API key (give it to %s; it is not stored):\n  %s\nConfig entry for \"keys\":\n  {\"name\": %q, \"sha256\": %q}\n", args[0], key, args[0], hex.EncodeToString(sum[:]))
	return nil
}
