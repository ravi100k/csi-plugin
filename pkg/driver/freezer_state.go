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

const frozenTargetsConfigMap = "hammerspace-csi-frozen-targets"

type frozenTargetStore interface {
	Save(context.Context, FrozenTarget) error
	Delete(context.Context, FrozenTarget) error
	List(context.Context) ([]FrozenTarget, error)
}

type kubeFrozenTargetStore struct {
	clientset kubernetes.Interface
	namespace string
}

func frozenTargetKey(target FrozenTarget) string {
	sum := sha256.Sum256([]byte(target.NodeName + "\x00" + target.MountPath))
	return hex.EncodeToString(sum[:])
}

func (s *kubeFrozenTargetStore) Save(ctx context.Context, target FrozenTarget) error {
	encoded, err := json.Marshal(target)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, frozenTargetsConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[frozenTargetKey(target)] = string(encoded)
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func (s *kubeFrozenTargetStore) Delete(ctx context.Context, target FrozenTarget) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, frozenTargetsConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		delete(cm.Data, frozenTargetKey(target))
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func (s *kubeFrozenTargetStore) List(ctx context.Context) ([]FrozenTarget, error) {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, frozenTargetsConfigMap, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	targets := make([]FrozenTarget, 0, len(cm.Data))
	for key, encoded := range cm.Data {
		var target FrozenTarget
		if err := json.Unmarshal([]byte(encoded), &target); err != nil {
			return nil, fmt.Errorf("decode frozen target %s: %w", key, err)
		}
		targets = append(targets, target)
	}
	return targets, nil
}
