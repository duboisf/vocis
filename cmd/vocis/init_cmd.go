package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"vocis/internal/config"
)

var initForce bool

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create default config file",
	Long: `Create a default config file. If a config already exists, writes the new
defaults to config.new.yaml and opens Neovim in diff mode so you can merge.
Use --force to overwrite without diffing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runInit(initForce)
	},
}

func init() {
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite existing config with defaults")
}

func runInit(force bool) error {
	path, err := config.Path()
	if err != nil {
		return err
	}

	if force {
		if err := config.Save(path, config.Default()); err != nil {
			return err
		}
		fmt.Printf("wrote %s (forced)\n", path)
		return nil
	}

	// Config doesn't exist yet — create it.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := config.Save(path, config.Default()); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
		return nil
	}

	// Config exists — write defaults to .new file and diff.
	newPath := path + ".new"
	if err := config.Save(newPath, config.Default()); err != nil {
		return err
	}
	fmt.Printf("wrote new defaults to %s\n", newPath)
	fmt.Printf("opening diff: %s (current) vs %s (new defaults)\n", path, newPath)

	cmd := exec.Command("nvim", "-d", "-c", "set diffopt+=iwhite", path, newPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nvim: %w", err)
	}

	// Clean up the .new file after the user closes nvim.
	if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cleanup %s: %w", newPath, err)
	}
	fmt.Println("cleaned up", newPath)
	return nil
}
