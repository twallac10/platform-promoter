# platform-promoter

Evidence-gated promoter for platform apps. A change moves to tier N only once
it is proven in tier N-1; the promotion PR is generated with that evidence.

```
promoter check   --app kyverno --tier dev    # gate + record a Deployment in the dev environment
promoter promote --app kyverno --from dev    # gate dev, then open the stg PR
promoter checks                              # list check sets
```

Flags: `--fleet <dir|raw-url>` (tiers.yaml/apps.yaml; env `PROMOTER_FLEET`),
`--context <ctx>` (check only this cluster of the tier), `--no-record` (dry run:
no Deployments, comments or Events; reads public repos without a token).
Token: `PROMOTER_TOKEN` (fine-grained, app repos only: contents, pull
requests, deployments write). Exit codes: 0 pass, 1 gate failed, 2 error.
From platform-fleet: `task check -- --app kyverno --tier dev`.

## What `check` does (per cluster in the tier)

1. Reads `tiers/<tier>/release.yaml` from the app repo's `main` at the commit
   that last changed it: the release the tier *should* run.
2. Starts a GitHub Deployment (environment = tier, ref = the commit the
   cluster applied, payload = app, cluster, chart digest, version), status
   `in_progress`.
3. Runs the checks concurrently. A check that lacks data returns *pending* and
   is asked again every `pollInterval`, up to `maxWait`; still pending then =
   fail. No soak timer.
4. Posts the evidence table as a commit comment (with the recorded metrics in
   a hidden JSON marker, the baseline for the next run), sets the Deployment to
   `success`/`failure` with a one-line summary and a link to the comment, and
   records a Kubernetes Event (`PromotionGatePassed` / `PromotionGateFailed`)
   on the app's HelmRelease.

## What `promote` adds

Runs `check` on the source tier. If every cluster in it has a `success`
Deployment for the same digest, and the source `release.yaml` on main is
still exactly what was checked, it points `promote/<to>/<short-digest>` at
main plus one commit that copies the source tier's `release.yaml` to
`tiers/<to>/release.yaml`, and opens (or updates) a PR labelled `promotion`
with the evidence tables. A failed gate opens nothing.

## In-cluster: the PromotionGate operator (`promoter serve`)

Deployed by platform-fleet to every cluster (`promoter/`). One
`PromotionGate` per app (generated from apps.yaml, identical in every
cluster); the operator's tier and cluster come from its env.

```
$ kubectl -n promoter get promotiongates
NAME                    APP                     TIER  VERSION  PHASE     MESSAGE
kyverno                 kyverno                 dev   3.9.3    Passed    promotion PR opened: https://github.com/...
kube-prometheus-stack   kube-prometheus-stack   dev   93.0.1   Failed    1 failed: canary scraped: no answer within 5m0s: canary target not discovered
```

- **Watches** the app's Flux Kustomization (`flux-system/<app>`) and
  HelmRelease (`<ns>/<app>`); a change in what Flux applied or in readiness
  reconciles the gate. Every gate is also re-evaluated each `RESYNC_INTERVAL`.
- **Decides from GitHub** (authoritative: the release main declares for this
  tier, and this cluster's latest gate Deployment), so restarts are safe:
  new digest → gate; failed → retry 2m after Flux applies something new, else
  every 30m; stale `in_progress` → 15m; passed but next tier behind → retry
  the PR every 6h. It waits while Flux isn't Ready, but gates anyway after
  10m stuck, so a broken release is recorded as a failure.
- **Status** mirrors the latest run: phase, per-check results, evidence URL,
  the PR it opened, and a `GatePassed` condition.
- Gates only its own cluster (refuses any other), one replica with leader
  election, up to 4 gates in parallel.

Env: `TIER`, `CLUSTER_NAME`, `PROMOTER_FLEET`, `PROMOTER_TOKEN`,
`RESYNC_INTERVAL` (`10m`), `POD_NAMESPACE`. Health on `:8081`, metrics on
`:8080`. API types in `api/v1alpha1`; regenerate deepcopy and the CRD
(`config/crd/`) with `go generate ./api/...`, then copy the CRD to
`platform-fleet/promoter/crd.yaml` (`task lint` checks they match). The image
is built by `.github/workflows/image.yaml` on `v*` tags.

## CI: reusable guardrail workflow

`.github/workflows/guardrails.yaml`, called by each app repo's
`promotion.yaml` on every PR (pinned by tag). The required check is
`promotion / promotion-gate`. `promoter verify-pr` fails a promotion PR if:

- any file other than `tiers/<target>/release.yaml` changed
- the target `release.yaml` isn't byte-identical to the source tier's on main
- the source tier's latest gate Deployment isn't `success` for the same digest
  on every one of its clusters (entry tier: the digest must exist in GHCR)
- a `HelmRepository`, or an `OCIRepository` outside our GHCR prefix, appears anywhere

plus a gitleaks scan of the PR's commits. Promotions then wait in the target
tier's environment (reviewers where `requiresApproval`), and auto-merge.

## Checks

Every app gets the common checks; `checks:` in apps.yaml adds app sets.

| set | checks |
|---|---|
| common | Flux Kustomization Ready at/after the release commit · OCIRepository artifact = release digest and HelmRelease Ready with that chart (`history[0].ociDigest`, config digest) · running containers on the image tags the release pins · Helm tests passed (where enabled) |
| kyverno | webhooks registered with ready endpoints · all policies Ready · dry-run: known-good pod admitted, privileged pod denied (Deny) / admitted (Audit) · canary PolicyReport failures not above baseline |
| kyverno-policies | chart policies Ready · enforcement mode matches the HelmRelease · privileged pod denied/admitted per mode |
| guardrails | guard policies Ready · dry-run: HelmRepository denied, foreign OCIRepository denied, GHCR OCIRepository admitted |
| kube-prometheus-stack | Prometheus ready · canary scraped ≥ `minScrapes` times since Ready and up · all targets up · no rule evaluation failures · Grafana health + datasource health · a known dashboard query returns data |
| otel-collector | collector Deployment available · accepted spans increasing · export failures not increasing (two samples ≥ `sampleInterval` apart) |

### Adding a check

Implement `gate.Check` (`Name`, `Run(ctx, *gate.Env) gate.Result`) and
register a set in `internal/checks` with `Register(name, factory)`. Return
`Pending` while there isn't enough data to answer. The interface is small on
purpose: a check backed by an external system (e.g. a custom resource) just
reads its status and maps it to a `Result`.

## Layout

```
cmd/promoter         CLI
internal/gate        Check interface, runner (poll until answer or timeout), report/table
internal/checks      common + per-app checks, Prometheus/metrics helpers
internal/controller  PromotionGate reconciler (in-cluster operator)
api/v1alpha1         PromotionGate API (CRD in config/crd)
internal/cluster     kube clients, service HTTP (port-forward locally, DNS in-cluster), dry-run, Events
internal/ledger      GitHub: Deployments, evidence comments, promotion PRs
internal/promote     check / promote orchestration
internal/config      tiers.yaml / apps.yaml
internal/release     release.yaml parsing
```

`go test ./...` covers the runner, every check against fake clusters, the
promote flow against an in-memory ledger, and the GitHub calls against a fake API.
