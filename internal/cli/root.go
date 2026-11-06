// Package cli defines the cobra commands. It only parses arguments and
// prints; the work happens in workflow.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/i18n"
)

// Version is set at build time: -ldflags "-X composesafe/internal/cli.Version=v0.1.0".
var Version = "dev"

// app carries the state shared by all commands.
type app struct {
	out, errOut io.Writer
	runner      docker.Runner

	// flags
	files       []string
	project     string
	configPath  string
	jsonOut     bool
	noLLM       bool
	lang        string
	execute     bool
	yes         bool
	forceUnlock bool

	cfg *config.Config
	t   func(string, ...any) string
}

// NewRoot builds the command tree. The runner is injectable for tests.
func NewRoot(out, errOut io.Writer, runner docker.Runner) *cobra.Command {
	a := &app{out: out, errOut: errOut, runner: runner}

	root := &cobra.Command{
		Use:   "composesafe",
		Short: "A safety net for upgrading Docker Compose stacks: assess, snapshot, upgrade, verify, roll back",
		Long: "composesafe wraps a Compose upgrade with a risk assessment, a verified snapshot, " +
			"post-upgrade checks and a rollback path. Commands that change anything default to a dry run.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Name() == "version" || cmd.Name() == "help" {
				return nil
			}
			return a.loadConfig()
		},
	}
	root.SetOut(out)
	root.SetErr(errOut)

	f := root.PersistentFlags()
	f.StringArrayVarP(&a.files, "file", "f", nil, "compose file (repeatable); default: compose.yaml in the current directory")
	f.StringVarP(&a.project, "project-name", "p", "", "compose project name")
	f.StringVarP(&a.configPath, "config", "c", "", "path to composesafe.yaml")
	f.BoolVar(&a.jsonOut, "json", false, "machine-readable output where supported")
	f.BoolVar(&a.noLLM, "no-llm", false, "do not call an LLM even if enabled in the config")
	f.StringVar(&a.lang, "lang", "", "output language: en or zh")
	f.BoolVar(&a.execute, "execute", false, "actually make changes (commands that change anything are a dry run without it)")
	f.BoolVar(&a.yes, "yes", false, "skip the interactive confirmation (never overrides a BLOCK)")
	f.BoolVar(&a.forceUnlock, "force-unlock", false, "remove a stale project lock left by a dead process")

	root.AddCommand(
		a.newVersionCmd(),
		a.newScanCmd(),
		a.newPlanCmd(),
		a.newSnapshotCmd(),
		a.newUpgradeCmd(),
		a.newRollbackCmd(),
		a.newRehearseCmd(),
		a.newHistoryCmd(),
		a.newReportCmd(),
		a.newCleanupCmd(),
	)
	return root
}

// loadConfig resolves composesafe.yaml, then environment, then flags.
func (a *app) loadConfig() error {
	cwd, _ := os.Getwd()
	composeDir := cwd
	if len(a.files) > 0 {
		composeDir = filepath.Dir(a.files[0])
	}
	path, err := config.Find(a.configPath, composeDir, cwd)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return err
	}
	if a.lang != "" {
		cfg.Lang = a.lang
		if cfg.Lang != "en" && cfg.Lang != "zh" {
			return fmt.Errorf("--lang: %q is not one of en, zh", a.lang)
		}
	}
	if a.noLLM {
		cfg.LLM.Enabled = false
	}
	a.cfg = cfg
	a.t = i18n.T(cfg.Lang)
	if cfg.LLM.Enabled {
		// The setting is accepted so that a config file written for a later
		// version keeps working, but nothing reads it yet: say so, once.
		fmt.Fprintln(a.errOut, a.t("llm.unsupported"))
	}
	return nil
}

func (a *app) newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the composesafe version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprintln(a.out, "composesafe", Version)
			return err
		},
	}
}
