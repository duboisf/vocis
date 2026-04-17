package main

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:     "vocis",
	Short:   "vocis — voice-to-text for Linux (WOH-kiss)",
	Version: version,
	Long:    `vocis — Latin genitive of vox ("of voice"), pronounced WOH-kiss.

A Linux voice-to-text desktop helper. Hold a hotkey, speak, release to
transcribe and paste into the focused application.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServe()
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(doctorCmd)
	rootCmd.AddCommand(keyCmd)
}

func Execute() error {
	return rootCmd.Execute()
}
