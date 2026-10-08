package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestGeneratedCRDDefaultsMatchTheConstants reads the generated CRD and
// compares every default in it against the constant the reconciler falls back
// to.
//
// The two have to agree or an object created through the API server behaves
// differently from the same object handed to the reconciler directly, which
// is the gap between a controller test and production.
func TestGeneratedCRDDefaultsMatchTheConstants(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "ops.sam-farooq.dev_postgresbackups.yaml"))
	if err != nil {
		t.Fatalf("reading the generated CRD: %v", err)
	}
	var crd apiextv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parsing the generated CRD: %v", err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("the CRD has %d versions", len(crd.Spec.Versions))
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	cases := []struct {
		path []string
		want string
	}{
		{[]string{"backoffLimit"}, "2"},
		{[]string{"cleanupTimeoutSeconds"}, "900"},
		{[]string{"retention", "keepFailed"}, "3"},
		{[]string{"retention", "keepSucceeded"}, "7"},
		{[]string{"retention", "pruneFiles"}, "true"},
		{[]string{"concurrencyPolicy"}, `"Forbid"`},
		{[]string{"source", "port"}, "5432"},
		{[]string{"source", "sslMode"}, `"require"`},
		{[]string{"source", "usernameKey"}, `"username"`},
		{[]string{"source", "passwordKey"}, `"password"`},
	}
	for _, c := range cases {
		node := spec
		for _, step := range c.path {
			next, ok := node.Properties[step]
			if !ok {
				t.Fatalf("the CRD has no spec.%v", c.path)
			}
			node = next
		}
		if node.Default == nil {
			t.Errorf("spec.%v has no default in the CRD", c.path)
			continue
		}
		if got := string(node.Default.Raw); got != c.want {
			t.Errorf("spec.%v defaults to %s in the CRD, want %s", c.path, got, c.want)
		}
	}
}

func TestAccessorsMatchTheConstants(t *testing.T) {
	var spec PostgresBackupSpec
	if got := spec.BackoffLimitOrDefault(); got != DefaultBackoffLimit {
		t.Errorf("BackoffLimitOrDefault on an empty spec = %d", got)
	}
	if got := spec.CleanupTimeout().Seconds(); int64(got) != DefaultCleanupTimeoutSeconds {
		t.Errorf("CleanupTimeout on an empty spec = %vs", got)
	}

	var retention RetentionSpec
	if got := retention.KeepFailedOrDefault(); got != DefaultKeepFailed {
		t.Errorf("KeepFailedOrDefault on an empty spec = %d", got)
	}
	if got := retention.KeepSucceededOrDefault(); got != DefaultKeepSucceeded {
		t.Errorf("KeepSucceededOrDefault on an empty spec = %d", got)
	}
	if got := retention.PruneFilesOrDefault(); got != DefaultPruneFiles {
		t.Errorf("PruneFilesOrDefault on an empty spec = %v", got)
	}
}

func TestZeroIsExpressibleOnEveryPointerField(t *testing.T) {
	// The point of the pointers: zero has to survive a round trip through the
	// accessor rather than coming back as the default.
	zero32 := int32(0)
	zero64 := int64(0)
	off := false

	spec := PostgresBackupSpec{
		BackoffLimit:          &zero32,
		CleanupTimeoutSeconds: &zero64,
		Retention:             RetentionSpec{KeepFailed: &zero32, PruneFiles: &off},
	}
	if got := spec.BackoffLimitOrDefault(); got != 0 {
		t.Errorf("backoffLimit 0 came back as %d", got)
	}
	if got := spec.CleanupTimeout(); got != 0 {
		t.Errorf("cleanupTimeoutSeconds 0 came back as %s", got)
	}
	if got := spec.Retention.KeepFailedOrDefault(); got != 0 {
		t.Errorf("keepFailed 0 came back as %d", got)
	}
	if got := spec.Retention.PruneFilesOrDefault(); got {
		t.Error("pruneFiles false came back as true")
	}
}
