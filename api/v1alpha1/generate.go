package v1alpha1

// Regenerate deepcopy and the CRD (then copy it to platform-fleet/promoter/crd.yaml).
//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0 object paths=./... crd:crdVersions=v1 output:crd:dir=../../config/crd
