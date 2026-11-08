package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/twallac10/platform-promoter/api/v1alpha1"
	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/controller"
	"github.com/twallac10/platform-promoter/internal/ledger"
	"github.com/twallac10/platform-promoter/internal/promote"
)

// serve runs the PromotionGate operator in a tier cluster. It gates only the
// cluster it runs in (CLUSTER_NAME in tier TIER) and holds no credentials for
// any other cluster.
func serve(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fleetSrc := fs.String("fleet", envOr("PROMOTER_FLEET", ""), "platform-fleet dir or raw URL")
	tierName := fs.String("tier", os.Getenv("TIER"), "this cluster's tier")
	clusterName := fs.String("cluster", os.Getenv("CLUSTER_NAME"), "this cluster's name in tiers.yaml")
	namespace := fs.String("namespace", envOr("POD_NAMESPACE", "promoter"), "namespace of the PromotionGates")
	resync := fs.Duration("resync", envDuration("RESYNC_INTERVAL", 10*time.Minute), "re-evaluate every gate at least this often")
	concurrency := fs.Int("concurrency", 4, "gates run in parallel")
	metricsAddr := fs.String("metrics-addr", ":8080", "")
	probeAddr := fs.String("health-addr", ":8081", "")
	leaderElect := fs.Bool("leader-elect", true, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	log := ctrl.Log.WithName("promoter")
	if *fleetSrc == "" || *tierName == "" || *clusterName == "" {
		return die(errors.New("serve needs --fleet/PROMOTER_FLEET, --tier/TIER and --cluster/CLUSTER_NAME"))
	}
	token := os.Getenv("PROMOTER_TOKEN")
	if token == "" {
		return die(errors.New("PROMOTER_TOKEN is required"))
	}
	loadFleet := controller.FleetLoader(*fleetSrc, 5*time.Minute)
	fleet, err := loadFleet(ctx)
	if err != nil {
		return die(err)
	}
	tier, err := fleet.Tier(*tierName)
	if err != nil {
		return die(err)
	}
	if !contains(tier.Clusters, *clusterName) {
		return die(fmt.Errorf("cluster %q is not in tier %s (%v)", *clusterName, tier.Name, tier.Clusters))
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress:  *probeAddr,
		LeaderElection:          *leaderElect,
		LeaderElectionID:        "platform-promoter",
		LeaderElectionNamespace: *namespace,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&v1.PromotionGate{}: {Namespaces: map[string]cache.Config{*namespace: {}}},
		}},
	})
	if err != nil {
		return die(err)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	own, err := cluster.FromContext("") // in-cluster
	if err != nil {
		return die(err)
	}
	own.Name = *clusterName
	gh := ledger.NewGitHub(fleet.Owner, token)
	p := &promote.Promoter{
		Fleet: fleet, Ledger: gh, Record: true,
		Contexts: []string{*clusterName}, // each tier attests for itself
		Connect: func(name string) (*cluster.Cluster, error) {
			if name != *clusterName {
				return nil, fmt.Errorf("refusing to reach cluster %s from %s", name, *clusterName)
			}
			return own, nil
		},
	}
	r := &controller.Reconciler{
		Client: mgr.GetClient(), Fleet: loadFleet, Ledger: gh, Gater: fleetAware{p, loadFleet},
		Tier: tier.Name, Cluster: *clusterName, Namespace: *namespace, Resync: *resync,
	}
	if err := r.SetupWithManager(mgr, *concurrency); err != nil {
		return die(err)
	}
	log.Info("starting", "tier", tier.Name, "cluster", *clusterName, "namespace", *namespace, "resync", resync.String())
	if err := mgr.Start(ctx); err != nil {
		return die(err)
	}
	return 0
}

// fleetAware gives each run a copy of the promoter with the current fleet
// config (reconciles run concurrently, so the shared one is never mutated).
type fleetAware struct {
	p    *promote.Promoter
	load func(context.Context) (*config.Fleet, error)
}

func (f fleetAware) fresh(ctx context.Context) *promote.Promoter {
	pp := *f.p
	if fl, err := f.load(ctx); err == nil {
		pp.Fleet = fl
	}
	return &pp
}

func (f fleetAware) Check(ctx context.Context, app, tier string) (*promote.Result, error) {
	return f.fresh(ctx).Check(ctx, app, tier)
}

func (f fleetAware) Promote(ctx context.Context, app, from string) (string, *promote.Result, error) {
	return f.fresh(ctx).Promote(ctx, app, from)
}

func envDuration(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
