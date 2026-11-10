// Command anode is the independent v4 node: serve, install and emit only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/zzjcool/anotify/cmd/anode/internal/app"
	"github.com/zzjcool/anotify/cmd/anode/internal/config"
	"github.com/zzjcool/anotify/cmd/anode/internal/install"
)

// Set by the release builder; both hello and --version use exactly these facts.
var version = "dev"
var commit = "unknown"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: anode {serve|install|emit} [flags]\nanode --version\nUse anode <command> --help for flags.")
}
func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		usage(errOut)
		return 2
	}
	if len(args) == 1 && args[0] == "--version" {
		_ = json.NewEncoder(out).Encode(struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		}{version, commit})
		return 0
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		usage(out)
		return 0
	}
	switch args[0] {
	case "emit":
		return emit(args[1:], out, errOut)
	case "serve":
		if err := serve(args[1:], errOut); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(errOut, "anode serve: %v\n", err)
			return 2
		}
		return 0
	case "install":
		if err := installCLI(args[1:], out, errOut); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(errOut, "anode install: %v\n", err)
			return 2
		}
		return 0
	default:
		fmt.Fprintf(errOut, "unknown command %q; expected serve, install or emit\n", args[0])
		return 2
	}
}
func serve(args []string, errOut io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(errOut)
	cfgPath := fs.String("config", "/etc/anode/anode.yaml", "strict anode.yaml path")
	dataDir := fs.String("data", "/var/lib/anode", "private data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("serve accepts flags only")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if err := install.EnsureDirectory(*dataDir); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	if err := app.EnsureDirs(*dataDir); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	restart := make(chan struct{}, 1)
	// Capture the service's environment, NOT any task's run.env. Execution whitelist remains in runner.
	env := os.Environ()
	argv := append([]string(nil), os.Args...)
	logger := log.New(errOut, "anode: ", log.LstdFlags)
	err = app.Serve(ctx, app.Options{Config: cfg, DataDir: *dataDir, Version: version, Commit: commit, Logf: logger.Printf, Restart: func() {
		select {
		case restart <- struct{}{}:
		default:
		}
		cancel()
	}})
	if err != nil {
		return fmt.Errorf("node: %w", err)
	}
	select {
	case <-restart:
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("restart executable: %w", err)
		}
		if err := syscall.Exec(exe, argv, env); err != nil {
			return fmt.Errorf("restart (supervisor may retry): %w", err)
		}
	default:
	}
	return nil
}
func installCLI(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var o install.Options
	fs.StringVar(&o.Prefix, "prefix", "", "stage all defaults under an absolute non-root directory")
	fs.StringVar(&o.ConfigPath, "config", "", "anode.yaml destination")
	fs.StringVar(&o.BinPath, "bin", "", "anode binary destination")
	fs.StringVar(&o.UnitPath, "unit", "", "anode.service destination")
	fs.StringVar(&o.DataPath, "data", "", "private data directory")
	fs.StringVar(&o.Name, "name", "", "existing Node name (required)")
	fs.StringVar(&o.Server, "server", "", "ws(s) hub URL (required)")
	fs.StringVar(&o.Token, "token", "", "existing Node identity token (required; prefer --token-file)")
	tokenFile := fs.String("token-file", "", "read existing identity token from a protected file")
	fs.BoolVar(&o.UpdateToken, "update-token", false, "explicitly replace the saved token; preserve all local configuration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("install accepts flags only")
	}
	if *tokenFile != "" {
		if o.Token != "" {
			return errors.New("use token OR token-file")
		}
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("token-file: %w", err)
		}
		o.Token = string(b)
		o.Token = trimToken(o.Token)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("binary source: %w", err)
	}
	o.Source = exe
	if err := install.Apply(o, out); err != nil {
		return fmt.Errorf("%w; have an administrator rerun anode install with these paths (no sudo is executed)", err)
	}
	return nil
}
