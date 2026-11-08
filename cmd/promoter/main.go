// promoter: evidence-gated promotion of platform apps through tiers.
//
//	promoter check   --app kyverno --tier dev     run the gate, record a Deployment
//	promoter promote --app kyverno --from dev     gate + open the next tier's PR
//
// Tiers, clusters (kube contexts) and app repos come from platform-fleet's
// tiers.yaml and apps.yaml (--fleet: a directory or a raw URL).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/twallac10/platform-promoter/internal/checks"
	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/ledger"
	"github.com/twallac10/platform-promoter/internal/promote"
)

const usage = `usage:
  promoter check   --app <app> --tier <tier> [flags]
  promoter promote --app <app> --from <tier> [flags]
  promoter checks                                  list registered check sets
  promoter verify-pr --fleet <dir|url> [--repo r] [--dir .] [--main-ref origin/main] [--labels a,b]
                                                   CI guardrails for a PR (no cluster access)
  promoter serve [--tier t --cluster c --namespace promoter --resync 10m]
                                                   in-cluster PromotionGate operator
                                                   (env TIER, CLUSTER_NAME, PROMOTER_TOKEN, PROMOTER_FLEET)

flags:
  --fleet <dir|url>   platform-fleet dir or raw URL with tiers.yaml/apps.yaml
                      (env PROMOTER_FLEET, default ../platform-fleet)
  --context <ctx>     only check this kube context of the tier (repeatable via commas)
  --no-record         don't write Deployments, evidence comments or Events
                      (GitHub is still read; works without a token on public repos)

GitHub token: PROMOTER_TOKEN (fine-grained: app repos, contents + pull requests
+ deployments write), falling back to GITHUB_TOKEN.

exit codes: 0 gate passed / PR opened, 1 gate failed, 2 error`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1], os.Args[2:]))
}

func run(ctx context.Context, cmd string, args []string) int {
	if cmd == "serve" {
		return serve(ctx, args)
	}
	if cmd == "verify-pr" {
		return verifyPR(ctx, args)
	}
	if cmd == "checks" {
		fmt.Println("common: flux kustomization, chart digest, image tags, helm tests")
		fmt.Println("sets:   " + strings.Join(checks.Names(), ", "))
		return 0
	}
	if cmd != "check" && cmd != "promote" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fleetSrc := fs.String("fleet", envOr("PROMOTER_FLEET", "../platform-fleet"), "")
	appName := fs.String("app", "", "")
	tierName := fs.String("tier", "", "")
	from := fs.String("from", "", "")
	only := fs.String("context", "", "")
	noRecord := fs.Bool("no-record", false, "")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *appName == "" || (cmd == "check" && *tierName == "") || (cmd == "promote" && *from == "") {
		fs.Usage()
		return 2
	}
	fleet, err := config.Load(*fleetSrc)
	if err != nil {
		return die(err)
	}
	token := envOr("PROMOTER_TOKEN", os.Getenv("GITHUB_TOKEN"))
	if token == "" && !*noRecord {
		return die(errors.New("set PROMOTER_TOKEN (or use --no-record for a read-only dry run)"))
	}
	p := &promote.Promoter{
		Fleet:   fleet,
		Ledger:  ledger.NewGitHub(fleet.Owner, token),
		Connect: cluster.FromContext,
		Record:  !*noRecord,
		Out:     os.Stdout,
	}
	if *only != "" {
		p.Contexts = strings.Split(*only, ",")
	}

	switch cmd {
	case "check":
		res, err := p.Check(ctx, *appName, *tierName)
		if err != nil {
			return die(err)
		}
		if !res.Passed() {
			return 1
		}
		return 0
	default:
		url, _, err := p.Promote(ctx, *appName, *from)
		var gf *promote.GateFailedError
		switch {
		case errors.As(err, &gf):
			fmt.Fprintln(os.Stderr, err)
			return 1
		case err != nil:
			return die(err)
		case url != "":
			fmt.Println("promotion PR:", url)
		}
		return 0
	}
}

func die(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 2
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
