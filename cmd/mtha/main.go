// Command mtha is an on-demand operator TUI for a MikroTik RouterOS VRRP
// pair: readiness, drift, sync, runtime-logic deployment and planned
// failover. See project.md for the full spec.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"mtha/internal/config"
	"mtha/internal/poll"
	"mtha/internal/routeros"
	"mtha/internal/ui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mtha:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath string
		pairName   string
		write      bool
		initConfig bool
	)

	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}

	flag.StringVar(&configPath, "config", defaultPath, "path to pair config file")
	flag.StringVar(&pairName, "pair", "", "pair name (required if the config defines more than one)")
	flag.BoolVar(&write, "write", false, "allow write operations (sync, deploy, failover); default is read-only")
	flag.BoolVar(&initConfig, "init", false, "write a commented sample pair file to -config and exit")
	flag.Parse()

	if initConfig {
		if err := config.WriteSample(configPath); err != nil {
			return err
		}
		fmt.Printf("wrote sample config to %s\n", configPath)
		fmt.Println("set MTHA_<PAIR>_<ROUTER>_PASSWORD for each router before running (e.g. MTHA_CORE_A_PASSWORD)")
		return nil
	}

	file, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if len(file.Pairs) == 0 {
		return fmt.Errorf("no pairs defined in %s", configPath)
	}

	if pairName == "" {
		if len(file.Pairs) > 1 {
			return fmt.Errorf("multiple pairs defined in %s; pass -pair (pair selection screen lands in a later milestone)", configPath)
		}
		pairName = file.Pairs[0].Name
	}

	pair, err := file.Pair(pairName)
	if err != nil {
		return err
	}

	pollers, err := buildPollers(pair)
	if err != nil {
		return err
	}

	model := ui.New(pair, write, pollers)
	program := tea.NewProgram(model)
	_, err = program.Run()
	return err
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
