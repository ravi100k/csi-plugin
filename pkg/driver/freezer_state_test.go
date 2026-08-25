package driver

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKubeFrozenTargetStoreRoundTrip(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: frozenTargetsConfigMap, Namespace: "kube-system"},
	})
	store := &kubeFrozenTargetStore{clientset: clientset, namespace: "kube-system"}
	target := FrozenTarget{NodeName: "node-a", MountPath: "/var/lib/kubelet/pods/a/mount"}

	if err := store.Save(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	targets, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].MountPath != target.MountPath {
		t.Fatalf("targets = %#v, want %#v", targets, target)
	}
	if err := store.Delete(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	targets, err = store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("deleted target remains: %#v", targets)
	}
}

type memoryFrozenTargetStore struct {
	targets []FrozenTarget
}

func (s *memoryFrozenTargetStore) Save(_ context.Context, target FrozenTarget) error {
	s.targets = append(s.targets, target)
	return nil
}

func (s *memoryFrozenTargetStore) Delete(_ context.Context, target FrozenTarget) error {
	for i := range s.targets {
		if frozenTargetKey(s.targets[i]) == frozenTargetKey(target) {
			s.targets = append(s.targets[:i], s.targets[i+1:]...)
			break
		}
	}
	return nil
}

func (s *memoryFrozenTargetStore) List(context.Context) ([]FrozenTarget, error) {
	return append([]FrozenTarget(nil), s.targets...), nil
}

func TestRecoverFrozenTargetsRetainsFailures(t *testing.T) {
	success := FrozenTarget{MountPath: "/mnt/success"}
	failure := FrozenTarget{MountPath: "/mnt/failure"}
	store := &memoryFrozenTargetStore{targets: []FrozenTarget{success, failure}}
	freezer := &Freezer{
		state: store,
		exec: func(_ context.Context, target FrozenTarget, op string) error {
			if op != "--unfreeze" {
				t.Fatalf("operation = %q, want --unfreeze", op)
			}
			if target.MountPath == failure.MountPath {
				return errors.New("node unavailable")
			}
			return nil
		},
	}

	if err := freezer.RecoverFrozenTargets(context.Background()); err == nil {
		t.Fatal("recovery returned nil despite a failed target")
	}
	if len(store.targets) != 1 || store.targets[0].MountPath != failure.MountPath {
		t.Fatalf("remaining targets = %#v, want only failed target", store.targets)
	}
}
