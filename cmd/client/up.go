package main

import (
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

// newUpCmd builds `burrow up`.
func newUpCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Run every service in burrow.yaml",
		Long: "Run every service in burrow.yaml.\n\n" +
			"The file is looked for in this order: --file <path>, ./burrow.yaml, burrow.yaml in the\n" +
			"user config directory. `server` and `token` in the file are optional; what is absent\n" +
			"comes from the sign-in (burrow login).",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
		},
	}
	cmd.Flags().String("file", "", "path to burrow.yaml")
	return cmd
}
