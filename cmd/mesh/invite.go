package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"agent-gateway/internal/identity"
)

// runInvite mints a pairing invitation for a gateway data directory.
//
// It reads and writes the data directory directly rather than talking to the
// gateway over HTTP, so it works while the gateway is running and adds no
// route to the frozen API surface. Only the CA certificate is read: minting an
// invitation never needs the CA private key.
func runInvite(ctx context.Context, args []string) error {
	cmd := newCommand("invite", "Mint a pairing invitation for a gateway data directory. Works while the gateway is running.")
	dataDir := cmd.flags.String("data-dir", "./gateway-data", "the gateway's data directory (the one passed to 'mesh server')")
	ttl := cmd.flags.Duration("ttl", defaultInviteTTL, "how long the invitation stays valid")
	count := cmd.flags.Int("count", 1, "how many invitations to mint")
	list := cmd.flags.Bool("list", false, "only report how many invitations are still usable")
	if err := cmd.flags.Parse(args); err != nil {
		return err
	}
	if _, err := cmd.logger(); err != nil {
		return err
	}
	if *ttl > identity.MaxInvitationTTL {
		return fmt.Errorf("--ttl %s exceeds the %s limit for a pairing secret", *ttl, identity.MaxInvitationTTL)
	}
	if *count < 1 {
		return fmt.Errorf("--count must be at least 1")
	}

	if *list {
		pending, used, err := identity.InvitationSummary(*dataDir)
		if err != nil {
			return err
		}
		fmt.Printf("pending: %d\nused:    %d\n", pending, used)
		return nil
	}

	for i := 0; i < *count; i++ {
		invitation, err := identity.IssueInvitation(*dataDir, *ttl, nil)
		if err != nil {
			return err
		}
		fmt.Printf("invitation:  %s (expires %s)\n", invitation.Token, invitation.ExpiresAt.Format(time.RFC3339))
	}

	fmt.Printf("\nthis secret is shown once and is stored only as a hash. use it on the machine you are adding:\n")
	fmt.Printf("  mesh pair --server https://<gateway-host>:8443 --ca %s --token <token> --dir ./node\n",
		filepath.Join(*dataDir, "ca.crt"))
	return nil
}
