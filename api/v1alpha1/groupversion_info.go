// Package v1alpha1 holds the PostgresBackup API. It is alpha because the
// retention fields will change once there is a second destination type, and a
// stored object whose schema moves under it is worse than a version number
// that says so.
//
// +kubebuilder:object:generate=true
// +groupName=ops.sam-farooq.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version this package registers.
	GroupVersion = schema.GroupVersion{Group: "ops.sam-farooq.dev", Version: "v1alpha1"}

	// SchemeBuilder collects the types in this package for a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this package to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
