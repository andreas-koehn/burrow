package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
)

const serviceFileName = "burrow.yaml"

// isFile reports whether path is an existing file.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// findServiceFile returns the burrow.yaml to use: the one given with --file,
// else ./burrow.yaml, else burrow.yaml in the user config directory.
func findServiceFile(d deps, given string) (string, error) {
	if given != "" {
		return given, nil
	}
	if isFile(serviceFileName) {
		return serviceFileName, nil
	}
	// The directory is the one the sign-in lives in by default; the global
	// --config names a file, not a directory, and does not move it.
	inUserDir := ""
	if p, err := d.userConfigPath(""); err == nil {
		inUserDir = filepath.Join(filepath.Dir(p), serviceFileName)
		if isFile(inUserDir) {
			return inUserDir, nil
		}
	}
	msg := "No burrow.yaml found. Looked in:\n  --file <path> (not given)\n  ./" + serviceFileName
	if inUserDir != "" {
		msg += "\n  " + inUserDir
	}
	return "", usageErrorf("%s", msg)
}

// runUp is `burrow up`: it runs every service of the file until it is stopped.
func runUp(cmd *cobra.Command, d deps) error {
	given, _ := cmd.Flags().GetString("file")
	path, err := findServiceFile(d, given)
	if err != nil {
		return err
	}
	fc, err := client.LoadFileConfig(path)
	if err != nil {
		return err
	}
	g, err := readGlobals(cmd, d)
	if err != nil {
		return err
	}
	userPath, err := userConfigFile(cmd, d)
	if err != nil {
		return err
	}
	creds, err := resolveCredentials(d, userPath, &fc)
	if err != nil {
		return err
	}
	return foreground(cmd, d, creds, fc.Tunnels, g)
}

// newUpCmd builds `burrow up`.
func newUpCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Run every service in burrow.yaml",
		Long: "Run every service in burrow.yaml.\n\n" +
			"The file is looked for in this order: --file <path>, ./burrow.yaml, burrow.yaml in the\n" +
			"user config directory. `server` and `token` in the file are optional; what is absent\n" +
			"comes from the sign-in (burrow login).\n\n" +
			"BURROW_SERVER and BURROW_TOKEN, when set, come before the values in the file; this differs\n" +
			"from `burrow connect --config`, which uses the file alone.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if underServiceManager() {
				// The Windows service: the manager starts and stops the run,
				// and the log lines go to a file next to the sign-in.
				logPath := ""
				if p, err := userConfigFile(cmd, d); err == nil {
					logPath = filepath.Join(filepath.Dir(p), "burrow.log")
				}
				return runUnderServiceManager(logPath, func(ctx context.Context) error {
					cmd.SetContext(ctx)
					return runUp(cmd, d)
				})
			}
			return runUp(cmd, d)
		},
	}
	cmd.Flags().String("file", "", "path to burrow.yaml")
	return cmd
}
