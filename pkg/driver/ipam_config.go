package driver

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/example/dra-poc/pkg/ipam"
)

// LoadIPAMPoolsFromConfigMap reads IPAM pool definitions from a ConfigMap.
// The ConfigMap is expected to be in the dra-system namespace with the name
// "dra-ipam-pools" and a data key "pools.json" containing a JSON array of pools.
func LoadIPAMPoolsFromConfigMap(clientset kubernetes.Interface) ([]ipam.Pool, error) {
	cm, err := clientset.CoreV1().ConfigMaps("dra-system").Get(context.Background(), "dra-ipam-pools", metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to load IPAM pools ConfigMap: %v (using empty pool list)", err)
		return []ipam.Pool{}, nil
	}

	poolsJSON, ok := cm.Data["pools.json"]
	if !ok {
		klog.Warningf("ConfigMap dra-ipam-pools missing 'pools.json' key")
		return []ipam.Pool{}, nil
	}

	var pools []ipam.Pool
	if err := json.Unmarshal([]byte(poolsJSON), &pools); err != nil {
		return nil, fmt.Errorf("failed to parse pools.json: %w", err)
	}

	klog.Infof("Loaded %d IPAM pools from ConfigMap", len(pools))
	return pools, nil
}
