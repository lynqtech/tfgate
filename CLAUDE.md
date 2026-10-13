# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`tfgate` is a Go CLI (cobra + controller-runtime) that reviews and approves [tofu-controller](https://github.com/flux-iac/tofu-controller) plans across Kubernetes contexts, plus replan, suspend, resume and stuck-plan fixing. It talks only to the Kubernetes API; it must not shell out to `kubectl`, `tfctl`, `flux` or `jq`. `README.md` documents flags, prompts and RBAC in detail.

The Go module is named `tfgate` (`go.mod`), so imports are `tfgate/internal/...`. The project lives in its own repository (`github.com/lynqtech/tfgate`); the module path was deliberately not renamed to match.

## Commands

```sh
make build                                           # bin/tfgate with the release ldflags
make check                                           # go vet + go test
make lint                                            # golangci-lint run (default linters, no config file)
go test -race ./...                                  # what CI runs
go test ./internal/app -run TestApprovePendingPlan   # single test
go run ./cmd/tfgate -c <context> --dry-run           # try against a real cluster without changes
```

`make help` lists all targets. CI (`.github/workflows/ci.yml`) runs `go mod tidy -diff`, `go vet`, `go test -race`, golangci-lint and `goreleaser check`. `.pre-commit-config.yaml` runs gitleaks, golangci-lint and `go vet` on commit and `go test` on push. Releases are built by GoReleaser on `v*` tags. The golangci-lint version is pinned in both CI and pre-commit, keep them in sync.

Tests use the controller-runtime fake client and need no cluster. A few `cmd/tfgate` tests point a kubeconfig at `127.0.0.1:1` to exercise the unreachable-cluster path; they only need a refused connection.

## Architecture

Data flow: `cmd/tfgate/main.go` parses flags into `app.Options`, loads the kubeconfig, resolves contexts, then hands an `app.App` to `Run`. `App` is built with injected `NewClient` and `Now` funcs, which is what makes the workflow testable with fake clients and a fixed clock.

- `internal/app`: the workflow. `Run` loops over contexts, `runContext` lists stacks (or gets one with `-s`), `runStack` decides between stuck handling, replan, suspend, resume or interactive `review`. Errors are counted per stack and the run continues; the count drives exit code 1.
- `internal/tf`: operations on the `Terraform` CRD (`tofu-controller/api/v1alpha2`), all in namespace `flux-system`. `Client` does List/Get/Approve/Replan/SetSuspend and honors `DryRun` by printing the intended patch instead of applying it. `Plans` reads the human-readable plan from labeled `tfplan-*` ConfigMaps (`storeReadablePlan: human`) or Secrets (`json`), reassembled from chunks, with a legacy unlabeled ConfigMap fallback. `stuck.go` implements stuck detection.
- `internal/kube`: kubeconfig lookup (`--kube`, then `$KUBECONFIG`, then `~/.kube/lynqtech/config`), context resolution (exact name, else substring match), scheme and client construction.
- `internal/flux`: sets the reconcile annotation on the OCIRepository `<stack>-iac`.
- `internal/ui`: colored output and prompts. `Ask` reads from `/dev/tty` so prompts work when stdout is piped. `Sanitize` strips terminal escape sequences, control characters and bidi overrides from text that comes from the cluster.

## Invariants to preserve

- **Approval safety.** The user must never approve a plan they did not see. `Approve` re-reads the object, refuses if `status.plan.pending` changed, and sends the `resourceVersion` of that read in the patch so the API server rejects it with a conflict if the object changed in between; `checkPlanID` in `plan.go` refuses a stored plan whose `savedPlan` annotation differs from the pending one; `review` withholds the `Y` option when the plan could not be read. `-y` never skips the approval prompt, only replan, suspend and resume.
- **Replan mirrors `tfctl replan`**: status patch (clear `plan.pending`, `lastPlannedRevision`, `lastAttemptedRevision`, set `Ready=False`/`ReplanRequested`, field manager `tf-controller`), then the `reconcile.fluxcd.io/requestedAt` annotation. It does not wait for the new plan.
- **Constant API call count per context.** A full run does one `List` of Terraform objects and one bulk `List` of plan ConfigMaps/Secrets (`Plans` in bulk mode); no per-stack `Get` except right before approval and for legacy ConfigMaps. `TestFullRunUsesFewAPICalls` guards this. The client rate limit is raised to QPS 50/burst 100 in `kube.Client` because the client-go default made ~150 stacks take ~28 s.
- **Every write path must go through the `DryRun` check** (`mergePatch`, `Replan`, `flux.ReconcileOCI`). `TestDryRunNeverWrites` runs each write scenario with `--dry-run` and fails on any write that reaches the API, so add a scenario there for every new write path.
- **Cluster text is sanitized before printing.** Plan text, the stack summary (including the Ready message) and the pending plan ID go through `ui.Sanitize`, so a stored plan cannot move the cursor, erase lines or overwrite what the user has already seen. Sanitize for display only; compare and approve the raw values. Colors in plans are lost on purpose.

## Upstream coupling

The tofu-controller API module is pinned in `go.mod`. When the controller in the clusters is upgraded, check upstream `tfctl/replan.go` and `api/plan` for changes to the replan logic or plan storage format.

## Local tooling

`graft/`, `bin/`, `.claude/` are gitignored; `graft/` holds generated code-graph cards, not source. `.serena/` is untracked project config for Serena.
