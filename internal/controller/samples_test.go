package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
)

// TestSamplesApplyCleanly creates every file in config/samples against a real
// API server with the generated CRD installed.
//
// The README tells people to run kubectl apply on these, and a sample with a
// field that the CRD renamed is the commonest kind of documentation rot: it
// looks right in a diff and fails on a cluster.
func TestSamplesApplyCleanly(t *testing.T) {
	c := env(t)
	ns := namespace(t, c)

	dir := filepath.Join("..", "..", "config", "samples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) == 0 {
		t.Fatal("config/samples is empty")
	}

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("reading the sample: %v", err)
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(raw, &obj.Object); err != nil {
				t.Fatalf("parsing the sample: %v", err)
			}
			obj.SetNamespace(ns)

			if err := c.Create(context.Background(), obj); err != nil {
				t.Fatalf("the API server rejected the sample: %v", err)
			}

			// Round trip it through the typed client as well, which is what
			// catches a field the sample spells differently from the Go tag.
			var typed opsv1alpha1.PostgresBackup
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: obj.GetName()}, &typed); err != nil {
				t.Fatalf("reading the sample back: %v", err)
			}
			if typed.Spec.Schedule == "" {
				t.Error("spec.schedule came back empty, so the sample's key did not land on the field")
			}
			if typed.Spec.Destination.ClaimName == "" {
				t.Error("spec.destination.claimName came back empty")
			}
		})
	}
}
