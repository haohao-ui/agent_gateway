package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"agent-gateway/internal/doctor"
)

func runDoctor(ctx context.Context, args []string) error {
	cmd := newCommand("doctor", "Inspect and diagnose node or server configuration, certificates, clock drift, and database integrity.")
	nodeDir := cmd.flags.String("node-dir", defaultNodeDir(), "path to node directory holding credentials and state (default: ~/.agent-mesh-node)")
	serverMode := cmd.flags.Bool("server-mode", false, "run diagnostics in server mode instead of node mode")
	dataDir := cmd.flags.String("data-dir", envString("MESH_DATA_DIR", "./gateway-data"), "path to gateway server data directory (when --server-mode is set)")
	jsonOutput := cmd.flags.Bool("json", false, "output results in JSON format")

	if err := cmd.flags.Parse(args); err != nil {
		return err
	}

	var report doctor.Report
	if *serverMode {
		report = doctor.DiagnoseServer(ctx, *dataDir)
	} else {
		report = doctor.DiagnoseNode(ctx, *nodeDir)
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("encode json report: %w", err)
		}
	} else {
		printHumanReport(report)
	}

	if !report.Healthy {
		return errors.New("one or more diagnostic checks failed")
	}
	return nil
}

func printHumanReport(rep doctor.Report) {
	fmt.Printf("Agent Gateway Doctor (mode: %s, target: %s)\n", rep.Mode, rep.Target)
	fmt.Println("------------------------------------------------------------")

	for _, check := range rep.Checks {
		var icon string
		switch check.Status {
		case doctor.StatusOK:
			icon = "[✓]"
		case doctor.StatusWarning:
			icon = "[!]"
		case doctor.StatusFail:
			icon = "[✗]"
		default:
			icon = "[?]"
		}

		fmt.Printf("%s %-22s: %s\n", icon, check.Name, check.Message)
		if check.Remediation != "" && check.Status != doctor.StatusOK {
			fmt.Printf("    Remediation: %s\n", check.Remediation)
		}
	}

	fmt.Println("------------------------------------------------------------")
	if rep.Healthy {
		fmt.Println("Result: HEALTHY")
	} else {
		fmt.Println("Result: UNHEALTHY")
	}
}
