package driver

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKubeSnapshotRecordStoreRoundTrip(t *testing.T) {
	const namespace = "kube-system"
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: snapshotStateConfigMap, Namespace: namespace},
		Data:       map[string]string{},
	})
	store := &kubeSnapshotRecordStore{clientset: client, namespace: namespace}
	record := snapshotRecord{
		Name: "snapshot-request", SnapshotID: "backend|/share/file",
		SourceVolumeID: "/share/file", CreationUnixNano: 1234,
	}

	if err := store.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != record {
		t.Fatalf("record = %#v, want %#v", got, record)
	}
	if err := store.DeleteBySnapshotID(context.Background(), record.SnapshotID); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(context.Background(), record.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("record remains after delete: %#v", got)
	}
}
