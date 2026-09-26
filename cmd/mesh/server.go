package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-gateway/internal/httpapi"
	"agent-gateway/internal/identity"
	"agent-gateway/internal/protocol"
	"agent-gateway/internal/taskstore"
)

const (
	defaultServerAddr    = "127.0.0.1:8443"
	defaultInviteTTL     = 15 * time.Minute
	defaultExpireSweep   = 5 * time.Second
	serverReadTimeout    = 5 * time.Second
	serverIdleTimeout    = 2 * time.Minute
	serverShutdownWindow = 10 * time.Second
)

// gatewayOptions configures one gateway process.
type gatewayOptions struct {
	Addr        string
	DataDir     string
	Hosts       []string
	Invitations int
	InviteTTL   time.Duration
	ExpireEvery time.Duration
	Log         *slog.Logger
}

// gatewayInfo is what a caller needs to reach and trust a running gateway.
type gatewayInfo struct {
	Addr            string
	CACertPath      string
	Fingerprint     string
	Invitations     []protocol.PairInvitation
	DBPath          string
	ProtocolVersion int
}

// serveGateway runs the gateway until ctx is cancelled, then shuts it down
// gracefully. It calls onReady once the listener is accepting connections, so a
// caller (or a test) never has to guess when the address is usable.
func serveGateway(ctx context.Context, opts gatewayOptions, onReady func(gatewayInfo)) error {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Addr == "" {
		opts.Addr = defaultServerAddr
	}
	if opts.DataDir == "" {
		opts.DataDir = "./gateway-data"
	}
	if opts.InviteTTL <= 0 {
		opts.InviteTTL = defaultInviteTTL
	}
	if opts.ExpireEvery <= 0 {
		opts.ExpireEvery = defaultExpireSweep
	}

	// The data directory holds the CA key and task payloads, so it is private
	// from the start.
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	dbPath := filepath.Join(opts.DataDir, "tasks.sqlite")
	store, err := taskstore.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open task store: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			opts.Log.Warn("closing the task store", "error", err)
		}
	}()

	ca, err := identity.LoadOrGenerateCA(opts.DataDir)
	if err != nil {
		return fmt.Errorf("load or create the CA: %w", err)
	}

	api := httpapi.NewServer(store, ca)
	tlsConfig, err := api.BuildTLSConfig(opts.Hosts)
	if err != nil {
		return fmt.Errorf("build the server TLS configuration: %w", err)
	}

	listener, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.Addr, err)
	}

	server := &http.Server{
		Handler:   api.Handler(),
		TLSConfig: tlsConfig,
		// ReadHeaderTimeout bounds a slow client's header phase; IdleTimeout
		// reclaims dead connections. There is deliberately no WriteTimeout: a
		// claim long poll is supposed to hold the response open, and it ends on
		// its own timer.
		ReadHeaderTimeout: serverReadTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

	info := gatewayInfo{
		Addr:            "https://" + listener.Addr().String(),
		CACertPath:      filepath.Join(opts.DataDir, "ca.crt"),
		Fingerprint:     ca.Fingerprint(),
		DBPath:          dbPath,
		ProtocolVersion: protocol.CurrentProtocolVersion,
	}
	for i := 0; i < opts.Invitations; i++ {
		invitation, err := ca.GenerateInvitation(opts.InviteTTL)
		if err != nil {
			return fmt.Errorf("generate an invitation: %w", err)
		}
		info.Invitations = append(info.Invitations, invitation)
	}

	// The store has no background goroutine of its own: whoever owns the
	// process drives lease expiry, which is what turns a stalled attempt into
	// unknown instead of leaving it leased forever.
	sweepCtx, stopSweep := context.WithCancel(ctx)
	defer stopSweep()
	var sweepDone sync.WaitGroup
	sweepDone.Add(1)
	go func() {
		defer sweepDone.Done()
		sweepLeases(sweepCtx, store, opts.ExpireEvery, opts.Log)
	}()

	serveErr := make(chan error, 1)
	go func() {
		// ServeTLS with empty file names uses the certificates already in the
		// TLS configuration.
		err := server.ServeTLS(listener, "", "")
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	opts.Log.Info("gateway listening", "addr", info.Addr, "data_dir", opts.DataDir)
	if onReady != nil {
		onReady(info)
	}

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	opts.Log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), serverShutdownWindow)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down the server: %w", err)
	}
	stopSweep()
	sweepDone.Wait()
	return <-serveErr
}

// sweepLeases moves lapsed leases to unknown on a fixed interval.
func sweepLeases(ctx context.Context, store *taskstore.Store, every time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			moved, err := store.Expire(ctx, time.Now().UTC())
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("lease sweep failed", "error", err)
				}
				continue
			}
			if moved > 0 {
				log.Info("lapsed leases moved to unknown", "count", moved)
			}
		}
	}
}

// certHostWarning reports the case that leaves an operator with an opaque
// "certificate is valid for 127.0.0.1, ::1" failure on the node: the gateway is
// reachable on a non-loopback address, but nothing in that address reached the
// server certificate. Binding to a specific address or passing --hosts fixes
// it; binding to a wildcard without --hosts cannot.
func certHostWarning(addr string, hosts []string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	switch host {
	// An empty host means every interface as well, the same as the wildcards.
	case "0.0.0.0", "::", "":
		if len(hosts) == 0 {
			return "this gateway is listening on every interface, but its certificate names only " +
				"localhost. machines on other hosts will fail with an x509 error: restart with " +
				"--hosts <the address they connect to>"
		}
		return ""
	case "localhost", "127.0.0.1", "::1":
		return ""
	default:
		// A concrete listen address is put into the certificate, so it matches.
		return ""
	}
}

// reachableAddr replaces a wildcard listen address with a name a client can
// actually dial: printing "https://[::]:8443" in the pairing hint is a
// copy-paste that cannot work from another machine.
func reachableAddr(addr string, hosts []string) string {
	host, port, err := net.SplitHostPort(strings.TrimPrefix(addr, "https://"))
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		if len(hosts) > 0 {
			host = hosts[0]
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
		} else {
			host = "127.0.0.1"
		}
	}
	return "https://" + net.JoinHostPort(host, port)
}

func runServer(ctx context.Context, args []string) error {
	cmd := newCommand("server", "Run the gateway: pairing endpoint and mTLS task API.")
	addr := cmd.flags.String("addr", defaultServerAddr, "listen address")
	dataDir := cmd.flags.String("data-dir", "./gateway-data", "directory for the CA, database and node records")
	hosts := cmd.flags.String("hosts", "", "extra comma-separated host or IP names for the server certificate")
	invitations := cmd.flags.Int("invitations", 1, "how many pairing invitations to print at startup")
	inviteTTL := cmd.flags.Duration("invite-ttl", defaultInviteTTL, "how long each invitation stays valid")
	expireEvery := cmd.flags.Duration("expire-sweep", defaultExpireSweep, "how often lapsed leases move to unknown")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}

	log, err := cmd.logger()
	if err != nil {
		return err
	}

	address := *addr
	hostList := splitList(*hosts)
	if host, _, err := net.SplitHostPort(address); err == nil && host != "" && host != "0.0.0.0" && host != "::" {
		hostList = append(hostList, host)
	}

	return serveGateway(ctx, gatewayOptions{
		Addr:        address,
		DataDir:     *dataDir,
		Hosts:       hostList,
		Invitations: *invitations,
		InviteTTL:   *inviteTTL,
		ExpireEvery: *expireEvery,
		Log:         log,
	}, func(info gatewayInfo) {
		fmt.Printf("listening:   %s\n", info.Addr)
		fmt.Printf("data dir:    %s\n", *dataDir)
		fmt.Printf("CA cert:     %s\n", info.CACertPath)
		fmt.Printf("CA SHA-256:  %s\n", info.Fingerprint)
		for _, invitation := range info.Invitations {
			fmt.Printf("invitation:  %s (expires %s)\n", invitation.Token, invitation.ExpiresAt.UTC().Format(time.RFC3339))
		}
		if pending, err := identity.PendingInvitations(*dataDir); err == nil && pending > 0 {
			fmt.Printf("pending:     %d invitation(s) from earlier runs are still usable\n", pending)
		}
		if warning := certHostWarning(address, hostList); warning != "" {
			fmt.Printf("\nwarning:     %s\n", warning)
		}
		if len(info.Invitations) > 0 {
			fmt.Printf("\npair a machine with:\n  mesh pair --server %s --ca %s --token <token> --dir ./node\n",
				reachableAddr(info.Addr, hostList), info.CACertPath)
		}
	})
}
