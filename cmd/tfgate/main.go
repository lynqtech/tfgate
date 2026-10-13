// Command tfgate reviews, approves, replans, suspends and resumes
// tofu-controller Terraform plans across Kubernetes contexts.
package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"tfgate/internal/app"
	"tfgate/internal/kube"
	"tfgate/internal/tf"
	"tfgate/internal/ui"
)

const long = `tfgate manages tofu-controller plans: review and approve pending plans,
replan, suspend, resume, or fix stuck plans.

Without options, tfgate iterates over all TF stacks in the current kubectl
context, shows each plan and asks whether a pending plan should be applied.`

// Set at build time via -ldflags by GoReleaser.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// Without a logger controller-runtime prints a stack trace after 30s.
	ctrllog.SetLogger(logr.Discard())

	u := ui.New()
	var opts app.Options
	var contexts string
	exitCode := 0

	cmd := &cobra.Command{
		Use:           "tfgate",
		Short:         "Review and approve tofu-controller plans",
		Long:          long,
		Version:       version + " (commit " + commit + ", built " + date + ")",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if contexts != "" {
				opts.Contexts = strings.Split(contexts, ",")
			}
			if err := opts.Validate(); err != nil {
				return err
			}
			exitCode = execute(cmd.Context(), u, opts)
			return nil
		},
	}

	cmd.SetArgs(args)

	f := cmd.Flags()
	f.StringVarP(&opts.Kubeconfig, "kube", "k", "", "kubeconfig to use, colon-separated lists allowed (default $KUBECONFIG, then "+kube.DefaultKubeconfig+")")
	f.BoolVarP(&opts.All, "all", "a", false, "iterate over all contexts")
	f.StringVarP(&contexts, "context", "c", "", "comma-separated contexts; a value that is not an exact context name\nselects every context containing it, e.g. -c stage")
	f.StringVarP(&opts.Stack, "stack", "s", "", "a distinct TF stack; if absent, iterate over all stacks")
	f.BoolVarP(&opts.FixStuck, "fix-stuck", "f", false, "go only over stuck TF objects and replan them")
	f.BoolVarP(&opts.Replan, "replan", "r", false, "replan the stack(s)")
	f.BoolVar(&opts.Suspend, "suspend", false, "suspend the stack(s)")
	f.BoolVar(&opts.Resume, "resume", false, "resume the stack(s)")
	f.BoolVar(&opts.DryRun, "dry-run", false, "print changes instead of applying them")
	f.BoolVarP(&opts.Yes, "yes", "y", false, "do not ask before replan, suspend or resume (approvals always ask)")
	f.DurationVar(&opts.StuckThreshold, "stuck-threshold", tf.DefaultStuckThreshold, "age after which a Progressing/Unknown plan counts as stuck")

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		u.Error("%v", err)
		u.Println()
		u.Println(cmd.UsageString())
		return 1
	}
	return exitCode
}

func execute(ctx context.Context, u *ui.UI, opts app.Options) int {
	cfg, err := kube.Load(kube.KubeconfigPaths(opts.Kubeconfig, os.Getenv("KUBECONFIG")))
	if err != nil {
		u.Error("%v", err)
		return 1
	}

	contexts, errs := kube.ResolveContexts(cfg.Contexts(), cfg.CurrentContext(), opts.Contexts, opts.All)
	for _, e := range errs {
		u.Error("%v", e)
	}

	scheme, err := kube.Scheme()
	if err != nil {
		u.Error("%v", err)
		return 1
	}

	a := &app.App{
		UI:   u,
		Opts: opts,
		NewClient: func(kubeContext string) (client.Client, error) {
			return cfg.Client(kubeContext, scheme)
		},
		Now: time.Now,
	}
	if a.Run(ctx, contexts)+len(errs) > 0 {
		return 1
	}
	return 0
}
