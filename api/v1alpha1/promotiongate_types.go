package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PromotionGateSpec selects the app this gate attests in this cluster. The
// app's repo, namespace and check sets come from platform-fleet's apps.yaml;
// the tier and cluster are the operator's (one operator per cluster).
type PromotionGateSpec struct {
	// App is the app's name in apps.yaml.
	// +kubebuilder:validation:MinLength=1
	App string `json:"app"`
	// Suspend stops gating and promotion for this app in this cluster.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ReleaseRef is a tier's release as declared on main.
type ReleaseRef struct {
	Digest  string `json:"digest"`
	Version string `json:"version,omitempty"`
	// Commit is the app-repo commit that last changed the tier's release.yaml.
	Commit string `json:"commit,omitempty"`
}

// CheckResult is one row of the evidence table.
type CheckResult struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=pass;fail;pending;skip
	Status string `json:"status"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// GateRun is the latest evidence gate run in this cluster.
type GateRun struct {
	Digest         string       `json:"digest"`
	Version        string       `json:"version,omitempty"`
	StartTime      metav1.Time  `json:"startTime"`
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	Passed         bool         `json:"passed"`
	Summary        string       `json:"summary,omitempty"`
	// EvidenceURL links the full table recorded on GitHub (also the
	// Deployment status log_url).
	EvidenceURL string        `json:"evidenceURL,omitempty"`
	Results     []CheckResult `json:"results,omitempty"`
	// ObservedRevision is the Flux state the gate ran against
	// (Kustomization revision + HelmRelease attempted revision/config).
	ObservedRevision string `json:"observedRevision,omitempty"`
}

// PromotionRef is the PR opened for the next tier.
type PromotionRef struct {
	TargetTier     string      `json:"targetTier"`
	PullRequestURL string      `json:"pullRequestURL,omitempty"`
	Time           metav1.Time `json:"time"`
}

// Phases.
const (
	PhaseWaiting   = "Waiting"  // nothing to gate yet / waiting on other clusters
	PhaseChecking  = "Checking" // gate running
	PhasePassed    = "Passed"   // passed; promotion pending or last tier
	PhaseFailed    = "Failed"   // gate failed; no PR
	PhasePromoted  = "Promoted" // next tier already has this release
	PhaseSuspended = "Suspended"
	PhaseError     = "Error" // could not evaluate (GitHub, config)
)

// Condition types.
const (
	// ConditionGatePassed: the latest gate for the declared release passed.
	ConditionGatePassed = "GatePassed"
)

type PromotionGateStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Tier               string `json:"tier,omitempty"`
	Cluster            string `json:"cluster,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// Message explains the phase in one line.
	Message string `json:"message,omitempty"`
	// Release is what main says this tier runs.
	Release *ReleaseRef `json:"release,omitempty"`
	// LastGate mirrors the latest gate run recorded on GitHub (authoritative).
	LastGate  *GateRun      `json:"lastGate,omitempty"`
	Promotion *PromotionRef `json:"promotion,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PromotionGate attests one app in this cluster: it runs the evidence gate
// when Flux reconciles the app, records the result on GitHub, and opens the
// next tier's PR once every cluster of the tier has passed.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pg
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.app`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.status.tier`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.release.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=0
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PromotionGate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PromotionGateSpec   `json:"spec,omitempty"`
	Status PromotionGateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PromotionGateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PromotionGate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PromotionGate{}, &PromotionGateList{})
}
