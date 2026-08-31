package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const snapshotStateConfigMap = "hammerspace-csi-snapshot-state"

type snapshotRecord struct {
	Name             string `json:"name"`
	SnapshotID       string `json:"snapshotId"`
	SourceVolumeID   string `json:"sourceVolumeId"`
	CreationUnixNano int64  `json:"creationUnixNano"`
}

type snapshotRecordStore interface {
	Get(context.Context, string) (*snapshotRecord, error)
	Save(context.Context, snapshotRecord) error
	DeleteBySnapshotID(context.Context, string) error
}

type kubeSnapshotRecordStore struct {
	clientset kubernetes.Interface
	namespace string
}

func snapshotRecordKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func (s *kubeSnapshotRecordStore) Get(ctx context.Context, name string) (*snapshotRecord, error) {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, snapshotStateConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	encoded, ok := cm.Data[snapshotRecordKey(name)]
	if !ok {
		return nil, nil
	}
	var record snapshotRecord
	if err := json.Unmarshal([]byte(encoded), &record); err != nil {
		return nil, fmt.Errorf("decode snapshot record %q: %w", name, err)
	}
	return &record, nil
}

func (s *kubeSnapshotRecordStore) Save(ctx context.Context, record snapshotRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, snapshotStateConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[snapshotRecordKey(record.Name)] = string(encoded)
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func (s *kubeSnapshotRecordStore) DeleteBySnapshotID(ctx context.Context, snapshotID string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, snapshotStateConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for key, encoded := range cm.Data {
			var record snapshotRecord
			if json.Unmarshal([]byte(encoded), &record) == nil && record.SnapshotID == snapshotID {
				delete(cm.Data, key)
			}
		}
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}
