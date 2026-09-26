// Command mesh is the gateway's single binary. It carries both roles of the
// protocol: the gateway side (server) and the machine side (pair, node), plus
// the operator commands needed to put work on the queue.
//
// The command is deliberately thin: everything it runs lives in internal/ so
// the same code paths are exercised by the tests.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const usage = `mesh is the agent gateway.

usage:
  mesh server [flags]        run the gateway: pairing, mTLS task API
  mesh invite [flags]        mint another pairing invitation for a running gateway
  mesh pair [flags]          enrol this machine with a gateway (node side)
  mesh node [flags]          run the task loop for this machine
  mesh task [flags] <verb>   submit, get or cancel a task

  mesh credential issue|revoke   manage operator credentials locally
  mesh device revoke            revoke a device as administrator

Common flags:
  --log-level debug|info|warn|error   (default info)

Run "mesh <command> --help" for the flags of one command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "server":
		err = runServer(ctx, os.Args[2:])
	case "invite":
		err = runInvite(ctx, os.Args[2:])
	case "pair":
		err = runPair(ctx, os.Args[2:])
	case "node":
		err = runNode(ctx, os.Args[2:])
	case "credential":
		err = runCredential(ctx, os.Args[2:])
	case "device":
		err = runDevice(ctx, os.Args[2:])
	case "task":
		err = runTask(ctx, os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "mesh: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "mesh:", err)
		os.Exit(1)
	}
}

// command bundles the pieces every subcommand parses the same way.
type command struct {
	flags *flag.FlagSet
	level string
}

func newCommand(name, help string) *command {
	flags := flag.NewFlagSet(name, flag.ExitOnError)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "usage: mesh %s\n\n%s\n\nflags:\n", name, help)
		flags.PrintDefaults()
	}
	cmd := &command{flags: flags}
	flags.StringVar(&cmd.level, "log-level", "info", "log level: debug, info, warn or error")
	return cmd
}

// logger builds the process logger from the parsed level.
func (c *command) logger() (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", c.level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})), nil
}

// splitList parses a comma-separated flag value, ignoring empty entries.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
