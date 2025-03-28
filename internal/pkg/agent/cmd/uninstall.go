// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent/internal/pkg/agent/application/paths"
	"github.com/elastic/elastic-agent/internal/pkg/agent/install"
	"github.com/elastic/elastic-agent/internal/pkg/cli"
	"github.com/elastic/elastic-agent/pkg/core/logger"
	"github.com/elastic/elastic-agent/pkg/utils"
)

func newUninstallCommandWithArgs(_ []string, streams *cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall Elastic Agent from this system",
		Long: `This command uninstalls the Elastic Agent permanently from this system.  The system's service manager will no longer manage Elastic agent.

Unless -f is used this command will ask confirmation before performing removal.
`,
		Run: func(c *cobra.Command, _ []string) {
			if err := uninstallCmd(streams, c); err != nil {
				fmt.Fprintf(streams.Err, "Error: %v\n%s\n", err, troubleshootMessage())
				logExternal(fmt.Sprintf("%s uninstall failed: %s", paths.BinaryName, err))
				os.Exit(1)
			}
		},
	}

	cmd.Flags().BoolP("force", "f", false, "Force overwrite the current and do not prompt for confirmation")
	cmd.Flags().String("uninstall-token", "", "Uninstall token required for protected agent uninstall")
	cmd.Flags().Bool("skip-fleet-audit", false, "Skip fleet audit/unenroll")

	return cmd
}

func uninstallCmd(streams *cli.IOStreams, cmd *cobra.Command) error {
	var err error

	isAdmin, err := utils.HasRoot()
	if err != nil {
		return fmt.Errorf("unable to perform command while checking for administrator rights, %w", err)
	}
	if !isAdmin {
		return fmt.Errorf("unable to perform command, not executed with %s permissions", utils.PermissionUser)
	}
	status, reason := install.Status(paths.Top())
	if status == install.NotInstalled {
		return fmt.Errorf("not installed")
	}
	if status == install.Installed && !paths.RunningInstalled() {
		return fmt.Errorf("can only be uninstalled by executing the installed Elastic Agent at: %s", install.ExecutablePath(paths.Top()))
	}

	force, _ := cmd.Flags().GetBool("force")
	uninstallToken, _ := cmd.Flags().GetString("uninstall-token")
	skipFleetAudit, _ := cmd.Flags().GetBool("skip-fleet-audit")
	if status == install.Broken {
		if !force {
			fmt.Fprintf(streams.Out, "%s is installed but currently broken: %s\n", paths.ServiceDisplayName(), reason)
			confirm, err := cli.Confirm(fmt.Sprintf("Continuing will uninstall the broken %s at %s. Do you want to continue?", paths.ServiceDisplayName(), paths.Top()), true)
			if err != nil {
				return fmt.Errorf("problem reading prompt response")
			}
			if !confirm {
				return fmt.Errorf("uninstall was cancelled by the user")
			}
		}
	} else {
		if !force {
			confirm, err := cli.Confirm(fmt.Sprintf("%s will be uninstalled from your system at %s. Do you want to continue?", paths.ServiceDisplayName(), paths.Top()), true)
			if err != nil {
				return fmt.Errorf("problem reading prompt response")
			}
			if !confirm {
				return fmt.Errorf("uninstall was cancelled by the user")
			}
		}
	}

	progBar := install.CreateAndStartNewSpinner(streams.Out, fmt.Sprintf("Uninstalling %s...", paths.ServiceDisplayName()))

	log, logBuff := logger.NewInMemory("uninstall", logp.ConsoleEncoderConfig())
	defer func() {
		if err == nil {
			return
		}
		fmt.Fprintf(os.Stderr, "Error uninstalling. Printing logs\n")
		fmt.Fprint(os.Stderr, logBuff.String())
	}()

	err = install.Uninstall(cmd.Context(), paths.ConfigFile(), paths.Top(), uninstallToken, log, progBar, skipFleetAudit)
	if err != nil {
		progBar.Describe("Failed to uninstall agent")
		return fmt.Errorf("error uninstalling agent: %w", err)
	} else {
		progBar.Describe("Done")
	}
	_ = progBar.Finish()
	_ = progBar.Exit()
	fmt.Fprintf(streams.Out, "\n%s has been uninstalled.\n", paths.ServiceDisplayName())

	_ = install.RemovePath(paths.Top())
	return nil
}
