package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
)

// The control plane is started once for the package and only when a test that
// needs it asks, so the pure tests in this package still run on a machine
// that has never downloaded the binaries.
//
// A missing binary is a failure and not a skip. A suite that skips its only
// integration tests when the assets are absent is a green tick that means
// nothing, and CI is exactly where the assets go missing.
var (
	envOnce    sync.Once
	testEnv    *envtest.Environment
	testCfg    *rest.Config
	testClient client.Client
	envErr     error
	scheme     = runtime.NewScheme()
)

func startEnv() {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		envErr = errors.New("KUBEBUILDER_ASSETS is not set; run `make test`, which calls setup-envtest first")
		return
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		envErr = err
		return
	}
	if err := opsv1alpha1.AddToScheme(scheme); err != nil {
		envErr = err
		return
	}

	testEnv = &envtest.Environment{
		// The CRD is loaded from the generated file, so these tests fail when
		// the generator output and the Go types disagree.
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		envErr = fmt.Errorf("starting the control plane: %w", err)
		return
	}
	testCfg = cfg

	// A direct client, not a cached one. Every read in these tests is
	// read-your-writes, which is what makes "call Reconcile twice and the
	// second call changes nothing" an exact assertion rather than a race
	// against an informer.
	testClient, envErr = client.New(cfg, client.Options{Scheme: scheme})
}

// env returns the test client, failing the test if the control plane is not
// available.
func env(t *testing.T) client.Client {
	t.Helper()
	envOnce.Do(startEnv)
	if envErr != nil {
		t.Fatalf("envtest unavailable: %v", envErr)
	}
	return testClient
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testEnv != nil {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stopping the control plane: %v\n", err)
		}
	}
	os.Exit(code)
}

// fakeClock lets a test stand either side of a slot. Every slot decision is a
// function of the clock, so this is the only thing a test has to move.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// namespace makes a fresh namespace per test. ownedJobs lists the whole
// namespace, so two tests sharing one would see each other's Jobs.
func namespace(t *testing.T, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pgb-"}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating a namespace: %v", err)
	}
	return ns.Name
}
