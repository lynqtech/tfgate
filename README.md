# tfgate

`tfgate` reviews and approves [tofu-controller](https://github.com/flux-iac/tofu-controller) plans across Kubernetes contexts. It also replans, suspends and resumes stacks, and detects and fixes stuck plans.

It needs neither `kubectl`, `tfctl`, `flux` nor `jq`: everything runs through the Kubernetes API.

## Why tfgate

The official tofu-controller CLI, `tfctl`, does not support multi-cluster approvals. It works against one cluster at a time, so approving the pending plans of a service that runs in several environments means switching context and repeating the same steps per cluster and per stack.

`tfgate` closes that gap. It selects many contexts in one run (`-a`, or `-c` with a list or a substring such as `stage`), lists the stacks with pending plans in each, shows every plan and asks for approval per stack. The review stays explicit: each plan is shown before approval, and `-y` never skips the approval prompt.

Replan, suspend, resume and stuck-plan fixing follow the same multi-cluster model. Replan behaves like `tfctl replan`.

## Prerequisites

- Go (version from `go.mod`) to build.
- A kubeconfig with access to the target clusters. Lookup order:
  1. `--kube <path>` (colon-separated lists allowed)
  2. `$KUBECONFIG`
  3. `~/.kube/lynqtech/config`
- RBAC in namespace `flux-system`:

  | Resource | Verbs |
  |---|---|
  | `terraforms.infra.contrib.fluxcd.io` | `get`, `list`, `patch` |
  | `terraforms.infra.contrib.fluxcd.io/status` | `patch` (replan) |
  | `ocirepositories.source.toolkit.fluxcd.io` | `get`, `patch` (OCI reconcile) |
  | `configmaps`, `secrets` | `list` (reading plans) |

## Install / build

```sh
go build -o bin/tfgate ./cmd/tfgate
# or
go install ./cmd/tfgate
```

### Shell completion

`tfgate completion <bash|zsh|fish|powershell>` prints a completion script. Flag names and the contexts of your kubeconfig (`-c`) are completed; after a comma, `-c a,<TAB>` completes the next context. `--kube` is honored when it comes before `-c`.

```sh
# zsh, add to ~/.zshrc
source <(tfgate completion zsh)
```

## Usage

```
tfgate [flags]
```

| Flag | Description |
|---|---|
| `-h, --help` | Show help |
| `-k, --kube <path>` | Kubeconfig to use |
| `-a, --all` | Iterate over all contexts |
| `-c, --context <list>` | Comma-separated contexts. A value that is not an exact context name selects every context containing it (`-c stage` = all stage contexts) |
| `-s, --stack <name>` | One TF stack. Without it, all stacks in `flux-system` |
| `-f, --fix-stuck` | Only handle stuck stacks and replan them without asking |
| `-r, --replan` | Replan the stack(s), asks per stack |
| `--suspend` | Suspend the stack(s), asks per stack |
| `--resume` | Resume the stack(s), asks per stack |
| `--dry-run` | Print every change instead of applying it |
| `--stuck-threshold <dur>` | Age after which a plan counts as stuck (default `2h`) |
| `-y, --yes` | Do not ask before replan, suspend or resume. Approvals always ask |

`--replan`, `--suspend` and `--resume` are mutually exclusive. `--all` and `--context` are mutually exclusive.

Before any work tfgate prints the contexts of the run when a `-c` entry was resolved by substring (the matching entry is shown per context), and when `--fix-stuck` runs, or `--replan`, `--suspend` or `--resume` runs with `-y`, since these change stacks without a per-stack prompt.

### Examples

```sh
# Review all stacks in the current context
tfgate

# One stack in two contexts
tfgate -c clientA-stage,clientA-prod -s my-service

# All stage contexts
tfgate -c stage

# Find and fix stuck plans everywhere
tfgate -a --fix-stuck

# See what would happen before touching prod
tfgate -c enercity-prod --dry-run
```

## Interactive prompts

In the default mode `tfgate` shows each stack's status and plan. If a plan is pending:

```
Found pending version plan-main-abc123. Should it be applied?
[Y=yes, R=replan, O=OCI reconciliation, any other key=skip]?
```

| Key | Action |
|---|---|
| `Y` | Approve: sets `spec.approvePlan` to the pending plan. Refused if the pending plan changed while the prompt was open |
| `R` | Replan, same as `tfctl replan` |
| `O` | Request reconciliation of the OCIRepository `<stack>-iac`, same as `flux reconcile source oci` |
| any other | Skip |

If the plan cannot be read, `Y` is not offered. The same applies if the stored plan belongs to a different version than the pending one (`savedPlan` annotation), so you never review one plan and approve another.

Plans are read like `tfctl show plan` does: from the labeled `tfplan-*` ConfigMaps (or Secrets for `storeReadablePlan: json`), reassembled from chunks. Unlabeled ConfigMaps written by older controller versions (`tfplan-<workspace>-<stack>`) are read as a fallback.

Prompts read from `/dev/tty`, so `tfgate` stays interactive when output is piped.

## Stuck detection

A stack is stuck if it has a condition with reason `Progressing` and status `Unknown` whose latest `lastTransitionTime` is older than `--stuck-threshold` (default 2h). Without `--fix-stuck`, stuck stacks are only reported. With `--fix-stuck`, other stacks are skipped and stuck ones are replanned without asking.

## What a replan does

Same as `tfctl replan`:

1. Status patch: clear `status.plan.pending`, `status.lastPlannedRevision`, `status.lastAttemptedRevision`, and set `Ready=False` with reason `ReplanRequested`.
2. Set the annotation `reconcile.fluxcd.io/requestedAt`.

Unlike `tfctl replan`, `tfgate` does not wait for the new plan. Run `tfgate -s <stack>` again to review it.

## Behavior notes

- Every call uses the selected context.
- `--kube` applies everywhere, and `$KUBECONFIG` lists work.
- Replan, suspend and resume run synchronously and report errors.
- Errors are reported per stack and the run continues.
- Conflicting action flags are rejected.
- Exit code is 1 if any error occurred.
- Approval verifies that the pending plan is still the one shown.

## Performance

A run over a context needs a constant number of API calls, independent of the number of stacks:

- one `List` of all Terraform objects; their data is used directly, without a `Get` per stack
- one `List` of all plan ConfigMaps, on the first pending plan
- a `Get` only right before an approval (to verify the pending plan) and for legacy plan ConfigMaps

The client-side rate limit is raised to 50 requests/s (burst 100); the client-go default of 5 requests/s made a context with ~150 stacks take ~28 s. Now it takes under 1 s.

With `-s`, only that stack and its plan are fetched.

## Development

```
cmd/tfgate/main.go         flags and wiring
internal/app/              workflow over contexts and stacks, prompts
internal/kube/             kubeconfig loading, context resolution, client
internal/tf/               Terraform operations, stuck detection, plan reading
internal/flux/             OCIRepository reconcile annotation
internal/ui/               colored output, prompts
```

```sh
go test ./...
go vet ./...
```

The tests use the controller-runtime fake client, so they need no cluster. To try against a real cluster without changing anything:

```sh
go run ./cmd/tfgate -c lynqtech-playground --dry-run
```

The tofu-controller API module is pinned in `go.mod`. When the controller in the clusters is upgraded, check `tfctl/replan.go` and `api/plan` upstream for changes to the replan logic or the plan storage format.

## Safety notes

- Approving a plan applies real Tofu changes. Check the context and stack in the red `->` line and the pending version before pressing `Y`.
- Use `--dry-run` first on prod contexts.
- `-y` never applies to approvals.
