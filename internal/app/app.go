// Package app implements the tfgate workflow over contexts and stacks.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lynqtech/tfgate/internal/flux"
	"github.com/lynqtech/tfgate/internal/tf"
	"github.com/lynqtech/tfgate/internal/ui"
)

const separator = "++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++"

// Options are the parsed command line flags.
type Options struct {
	Kubeconfig     string
	All            bool
	Contexts       []string
	Stack          string
	FixStuck       bool
	Replan         bool
	Suspend        bool
	Resume         bool
	DryRun         bool
	Yes            bool
	StuckThreshold time.Duration
}

// Validate rejects flag combinations with unclear meaning.
func (o Options) Validate() error {
	var actions []string
	if o.Replan {
		actions = append(actions, "--replan")
	}
	if o.Suspend {
		actions = append(actions, "--suspend")
	}
	if o.Resume {
		actions = append(actions, "--resume")
	}
	if len(actions) > 1 {
		return fmt.Errorf("%s are mutually exclusive", strings.Join(actions, ", "))
	}
	if o.FixStuck && (o.Suspend || o.Resume) {
		return errors.New("--fix-stuck cannot be combined with --suspend or --resume")
	}
	if o.All && len(o.Contexts) > 0 {
		return errors.New("--all and --context are mutually exclusive")
	}
	if o.StuckThreshold <= 0 {
		return errors.New("--stuck-threshold must be positive")
	}
	return nil
}

// App runs the workflow. NewClient builds a Kubernetes client per context.
type App struct {
	UI        *ui.UI
	Opts      Options
	NewClient func(kubeContext string) (client.Client, error)
	Now       func() time.Time
	// Substring maps contexts that were selected by substring match to the
	// entry that matched them, see kube.ResolveContexts. Optional.
	Substring map[string]string
}

// Run processes all contexts and returns the number of errors encountered.
func (a *App) Run(ctx context.Context, contexts []string) int {
	if len(a.Substring) > 0 || a.writesWithoutPrompt() {
		a.printContexts(contexts)
	}
	errCount := 0
	for _, kubeContext := range contexts {
		errCount += a.runContext(ctx, kubeContext)
	}
	return errCount
}

// writesWithoutPrompt reports whether the run changes stacks without asking
// for each one: fix-stuck never asks, replan, suspend and resume do with -y.
func (a *App) writesWithoutPrompt() bool {
	if a.Opts.FixStuck {
		return true
	}
	return a.Opts.Yes && (a.Opts.Replan || a.Opts.Suspend || a.Opts.Resume)
}

// printContexts lists the contexts of this run so the user sees the fan-out,
// and which of them were picked by substring rather than by exact name.
func (a *App) printContexts(contexts []string) {
	a.UI.Println("Contexts of this run:")
	for _, kubeContext := range contexts {
		if entry, ok := a.Substring[kubeContext]; ok {
			a.UI.Printf("  %s (matched by substring %q)\n", kubeContext, entry)
		} else {
			a.UI.Printf("  %s\n", kubeContext)
		}
	}
}

func (a *App) runContext(ctx context.Context, kubeContext string) int {
	kube, err := a.NewClient(kubeContext)
	if err != nil {
		a.UI.Error("%v", err)
		return 1
	}
	c := &tf.Client{Kube: kube, DryRun: a.Opts.DryRun, Out: a.UI.Out, Now: a.Now}

	if a.Opts.Stack != "" {
		a.printHeader(kubeContext, a.Opts.Stack)
		obj, err := c.Get(ctx, a.Opts.Stack)
		if apierrors.IsNotFound(err) {
			err = fmt.Errorf("terraform resource %s not found in %s", a.Opts.Stack, tf.Namespace)
		}
		if err == nil {
			err = a.runStack(ctx, c, tf.NewPlans(kube, false), kubeContext, obj)
		}
		if err != nil {
			a.UI.Error("%s/%s: %v", kubeContext, a.Opts.Stack, err)
			return 1
		}
		return 0
	}

	a.UI.Println("Iterating over all TF stacks in context:", kubeContext)
	objs, err := c.List(ctx)
	if err != nil {
		a.UI.Error("context %s: %v", kubeContext, err)
		return 1
	}

	// Plans of all stacks are fetched with one call on first use.
	plans := tf.NewPlans(kube, true)
	errCount := 0
	for i := range objs {
		a.printHeader(kubeContext, objs[i].Name)
		if err := a.runStack(ctx, c, plans, kubeContext, &objs[i]); err != nil {
			a.UI.Error("%s/%s: %v", kubeContext, objs[i].Name, err)
			errCount++
		}
	}
	return errCount
}

func (a *App) printHeader(kubeContext, stack string) {
	a.UI.Println()
	a.UI.Println(separator)
	a.UI.Red("-> %s / %s", kubeContext, stack)
}

func (a *App) runStack(ctx context.Context, c *tf.Client, plans *tf.Plans, kubeContext string, obj *tf.Terraform) error {
	stack := obj.Name

	stuck, since := tf.Stuck(obj.Status.Conditions, a.Now(), a.Opts.StuckThreshold)
	if stuck {
		a.UI.Yellow("Service stack %s is stuck since %s", stack, since.UTC().Format(time.RFC3339))
		if !a.Opts.Replan && !a.Opts.Suspend && !a.Opts.Resume && !a.Opts.FixStuck {
			a.UI.Yellow("Please rerun with --fix-stuck to fix this.")
		}
	}
	if a.Opts.FixStuck && !stuck {
		a.UI.Printf("Service stack %s not stuck\n", stack)
		return nil
	}

	a.UI.Println(ui.Sanitize(tf.Summary(obj, a.Now())))

	switch {
	case a.Opts.Replan || a.Opts.FixStuck:
		if a.Opts.FixStuck && stuck {
			a.UI.Yellow("Triggering replan of stuck plan...")
		} else if !a.confirm("Triggering replan") {
			return nil
		}
		return a.replan(ctx, c, stack)
	case a.Opts.Suspend:
		if !a.confirm("Suspend stack") {
			return nil
		}
		return c.SetSuspend(ctx, stack, true)
	case a.Opts.Resume:
		if !a.confirm("Resume stack") {
			return nil
		}
		return c.SetSuspend(ctx, stack, false)
	}
	return a.review(ctx, c, plans, kubeContext, obj)
}

// review shows the plan and asks what to do with a pending plan.
func (a *App) review(ctx context.Context, c *tf.Client, plans *tf.Plans, kubeContext string, obj *tf.Terraform) error {
	a.UI.Println("PLAN -------------------------------------------------------------------------")
	planText, planErr := plans.Readable(ctx, obj)
	if planErr != nil {
		a.UI.Error("cannot show plan: %v", planErr)
	} else {
		a.UI.Println(ui.Sanitize(planText))
	}
	defer a.UI.Println("------------------------------------------------------------------------------")

	pending := strings.TrimSpace(obj.Status.Plan.Pending)
	if pending == "" {
		return planErr
	}

	a.UI.Red("-> %s / %s", kubeContext, obj.Name)
	// pending is printed in the prompts, so it is sanitized for display only.
	// The raw value is what gets compared and approved.
	shown := ui.Sanitize(pending)
	var answer string
	if planErr != nil {
		// Never offer approval for a plan the user could not read.
		answer = a.UI.Ask(fmt.Sprintf("Found pending version %s, but the plan could not be shown.\n[R=replan, O=OCI reconciliation, any other key=skip]?", shown))
		if answer == "Y" {
			answer = ""
		}
	} else {
		answer = a.UI.Ask(fmt.Sprintf("Found pending version %s. Should it be applied?\n[Y=yes, R=replan, O=OCI reconciliation, any other key=skip]?", shown))
	}

	switch answer {
	case "Y":
		if err := c.Approve(ctx, obj.Name, obj.Status.Plan.Pending, planText); err != nil {
			return err
		}
		if !a.Opts.DryRun {
			a.UI.Printf("Approved %s for %s/%s\n", shown, tf.Namespace, obj.Name)
		}
	case "R":
		return a.replan(ctx, c, obj.Name)
	case "O":
		source := obj.Name + "-iac"
		if err := flux.ReconcileOCI(ctx, c.Kube, tf.Namespace, source, a.Now(), a.Opts.DryRun, a.UI.Out); err != nil {
			return err
		}
		if !a.Opts.DryRun {
			a.UI.Printf("Reconcile requested for ocirepository %s/%s\n", tf.Namespace, source)
		}
	default:
		a.UI.Println("skipping ........")
	}
	return planErr
}

func (a *App) replan(ctx context.Context, c *tf.Client, stack string) error {
	if err := c.Replan(ctx, stack); err != nil {
		return err
	}
	if !a.Opts.DryRun {
		a.UI.Printf("Replan requested for %s/%s\n", tf.Namespace, stack)
	}
	return nil
}

func (a *App) confirm(question string) bool {
	if a.Opts.Yes {
		return true
	}
	if a.UI.Ask(question+" [Y=yes, any=skip]?") == "Y" {
		return true
	}
	a.UI.Println("skipping ........")
	return false
}
