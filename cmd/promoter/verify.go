package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/ledger"
	"github.com/twallac10/platform-promoter/internal/verify"
)

// verifyPR is the CI guardrail (run by the reusable workflow on every PR).
// It needs a checkout of the PR head with main fetched, and a token that can
// read Deployments (GITHUB_TOKEN). Outputs go to $GITHUB_OUTPUT and the table
// to $GITHUB_STEP_SUMMARY when set.
func verifyPR(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("verify-pr", flag.ContinueOnError)
	fleetSrc := fs.String("fleet", envOr("PROMOTER_FLEET", ""), "platform-fleet dir or raw URL")
	repo := fs.String("repo", filepath.Base(os.Getenv("GITHUB_REPOSITORY")), "app repo name")
	dir := fs.String("dir", ".", "checkout of the PR head")
	mainRef := fs.String("main-ref", "origin/main", "git ref of the base branch")
	labels := fs.String("labels", "", "comma-separated PR labels")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *fleetSrc == "" || *repo == "" || *repo == "." {
		fmt.Fprintln(os.Stderr, "verify-pr needs --fleet and --repo (or GITHUB_REPOSITORY)")
		return 2
	}
	fleet, err := config.Load(*fleetSrc)
	if err != nil {
		return die(err)
	}
	changed, err := verify.ChangedFiles(*dir, *mainRef)
	if err != nil {
		return die(err)
	}
	o, err := verify.Verify(ctx, verify.Input{
		Fleet: fleet, Repo: *repo, Changed: changed, Labels: strings.Split(*labels, ","),
		HeadDir: *dir, Files: verify.GitFiles{Dir: *dir, MainRef: *mainRef},
		Ledger: ledger.NewGitHub(fleet.Owner, os.Getenv("GITHUB_TOKEN")),
		Reg:    verify.GHCR{},
	})
	if err != nil {
		return die(err)
	}
	md := o.Markdown()
	fmt.Println(md)
	appendFile(os.Getenv("GITHUB_STEP_SUMMARY"), "## Promotion guardrails\n\n"+md+"\n")
	appendFile(os.Getenv("GITHUB_OUTPUT"), fmt.Sprintf("is_promotion=%t\ntarget=%s\nsource=%s\nrequires_approval=%t\ndigest=%s\n",
		o.IsPromotion, o.Target, o.Source, o.RequiresApproval, o.Digest))
	if !o.Passed() {
		fmt.Fprintln(os.Stderr, "guardrails failed")
		return 1
	}
	return 0
}

func appendFile(path, s string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
		return
	}
	defer f.Close()
	f.WriteString(s)
}
