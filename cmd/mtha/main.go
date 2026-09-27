// Command mtha is an on-demand operator TUI for a MikroTik RouterOS VRRP
// pair: readiness, drift, sync, runtime-logic deployment and planned
// failover. See project.md for the full spec.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/poll"
	"mtha/internal/routeros"
	"mtha/internal/ui"
)

// version is the release this binary was built from. Release builds and
// `make build` set it with -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mtha:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}

	opts, err := parseFlags(flag.CommandLine, os.Args[1:], defaultPath)
	if err != nil {
		return err
	}

	model, err := setup(opts, os.Stdout)
	if err != nil || model == nil {
		return err
	}

	program := tea.NewProgram(model)
	_, err = program.Run()
	return err
}

// options are mtha's command-line flags.
type options struct {
	configPath string
	pairName   string
	write      bool
	initConfig bool
	showVer    bool
}

// parseFlags registers mtha's flags on fs and parses args into options. run
// passes the global flag.CommandLine and os.Args[1:], which is exactly what
// flag.Parse does; tests pass their own FlagSet.
func parseFlags(fs *flag.FlagSet, args []string, defaultPath string) (options, error) {
	var opts options
	fs.StringVar(&opts.configPath, "config", defaultPath, "path to pair config file")
	fs.StringVar(&opts.pairName, "pair", "", "pair name (required if the config defines more than one)")
	fs.BoolVar(&opts.write, "write", false, "allow write operations (sync, deploy, failover); default is read-only")
	fs.BoolVar(&opts.initConfig, "init", false, "write a commented sample pair file to -config and exit")
	fs.BoolVar(&opts.showVer, "version", false, "print the mtha version and exit")
	err := fs.Parse(args)
	return opts, err
}

// setup does everything run does short of starting the TUI. -version and
// -init print to out and return a nil model (nothing to run); otherwise it
// loads the pair file, selects the pair, builds its pollers and returns the
// root model.
func setup(opts options, out io.Writer) (tea.Model, error) {
	if opts.showVer {
		fmt.Fprintln(out, "mtha", buildVersion())
		return nil, nil
	}

	if opts.initConfig {
		if err := config.WriteSample(opts.configPath); err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "wrote sample config to %s\n", opts.configPath)
		fmt.Fprintln(out, "set MTHA_<PAIR>_<ROUTER>_PASSWORD for each router before running (e.g. MTHA_CORE_A_PASSWORD)")
		return nil, nil
	}

	file, err := config.Load(opts.configPath)
	if err != nil {
		return nil, err
	}
	for _, w := range file.Warnings {
		fmt.Fprintln(out, "warning:", w)
	}
	if len(file.Pairs) == 0 {
		return nil, fmt.Errorf("no pairs defined in %s", opts.configPath)
	}

	pairName := opts.pairName
	if pairName == "" {
		if len(file.Pairs) > 1 {
			return nil, fmt.Errorf("multiple pairs defined in %s; pass -pair to choose one", opts.configPath)
		}
		pairName = file.Pairs[0].Name
	}

	pair, err := file.Pair(pairName)
	if err != nil {
		return nil, err
	}

	pollers, err := buildPollers(pair)
	if err != nil {
		return nil, err
	}

	return ui.New(pair, opts.write, pollers), nil
}

// buildVersion is the stamped version, falling back to the module version
// Go records for `go install mtha/cmd/mtha@<tag>` builds, which skip ldflags.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func buildPollers(pair *config.Pair) (map[poll.RouterKey]*poll.Poller, error) {
	pollers := make(map[poll.RouterKey]*poll.Poller, len(pair.Routers))

	for key, router := range pair.Routers {
		password, err := config.ResolvePassword(pair.Name, key)
		if err != nil {
			return nil, err
		}

		client := routeros.New(routeros.Config{
			Host:        router.Host,
			Port:        router.Port,
			User:        router.User,
			Password:    password,
			InsecureTLS: router.InsecureTLS,
		})

		pollers[poll.RouterKey(key)] = poll.New(poll.RouterKey(key), client, ui.DefaultPollInterval())
	}

	return pollers, nil
}
