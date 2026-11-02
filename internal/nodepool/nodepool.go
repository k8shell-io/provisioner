// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Package nodepool discovers, from the cluster's nodes, the values of the
// configured node selector keys (e.g. k8shell.io/pool) and how many nodes
// carry each one.
package nodepool

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// syncTimeout bounds how long Start waits for the first node listing.
const syncTimeout = 30 * time.Second

// Value is one value of a selector key and the nodes carrying it.
type Value struct {
	Value            string
	NodeCount        int
	ReadyCount       int
	SchedulableCount int
}

// Key is a configured selector key with its discovered values, sorted by
// value.
type Key struct {
	Key    string
	Values []Value
}

// Discovery serves node selector options from a watch-backed node cache.
type Discovery struct {
	keys     []string
	client   kubernetes.Interface
	informer cache.SharedIndexInformer
}

// New returns a Discovery for the given keys. It does nothing until Start.
func New(client kubernetes.Interface, keys []string) *Discovery {
	return &Discovery{keys: keys, client: client}
}

// Start verifies the provisioner may read nodes, then starts the node
// informer and waits for it to sync. It returns an error when node access is
// denied or the cache does not sync in time, so the caller can fail startup.
// It is a no-op when no keys are configured.
func (d *Discovery) Start(ctx context.Context) error {
	if len(d.keys) == 0 {
		return nil
	}

	// A forbidden watch only makes the informer retry forever, so probe with a
	// list first to fail fast with the real cause.
	if _, err := d.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("list nodes (node selector discovery needs cluster-wide get/list/watch on nodes): %w", err)
	}

	d.informer = coreinformers.NewNodeInformer(d.client, 0, cache.Indexers{})
	_ = d.informer.SetTransform(slimNode)
	go d.informer.Run(ctx.Done())

	syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), d.informer.HasSynced) {
		return fmt.Errorf("node cache did not sync within %s", syncTimeout)
	}
	return nil
}

// Options returns every configured key, in configuration order, with the
// values currently present on nodes. It returns nil before Start succeeds.
func (d *Discovery) Options() []Key {
	if len(d.keys) == 0 {
		return nil
	}
	if d.informer == nil {
		return nil
	}
	var nodes []*corev1.Node
	for _, o := range d.informer.GetStore().List() {
		if n, ok := o.(*corev1.Node); ok {
			nodes = append(nodes, n)
		}
	}
	return summarize(d.keys, nodes)
}

// slimNode keeps only what discovery reads from a node, so the cache does not
// hold images, addresses and the rest of the node status.
func slimNode(obj interface{}) (interface{}, error) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return obj, nil
	}
	out := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            n.Name,
			Labels:          n.Labels,
			ResourceVersion: n.ResourceVersion,
		},
		Spec: corev1.NodeSpec{Unschedulable: n.Spec.Unschedulable},
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			out.Status.Conditions = []corev1.NodeCondition{{Type: c.Type, Status: c.Status}}
			break
		}
	}
	return out, nil
}

func isReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// summarize groups nodes by the values of keys. Nodes without a key are not
// counted for it.
func summarize(keys []string, nodes []*corev1.Node) []Key {
	out := make([]Key, 0, len(keys))
	for _, k := range keys {
		byValue := map[string]*Value{}
		for _, n := range nodes {
			v, ok := n.Labels[k]
			if !ok {
				continue
			}
			e := byValue[v]
			if e == nil {
				e = &Value{Value: v}
				byValue[v] = e
			}
			e.NodeCount++
			if isReady(n) {
				e.ReadyCount++
				if !n.Spec.Unschedulable {
					e.SchedulableCount++
				}
			}
		}
		vals := make([]Value, 0, len(byValue))
		for _, e := range byValue {
			vals = append(vals, *e)
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i].Value < vals[j].Value })
		out = append(out, Key{Key: k, Values: vals})
	}
	return out
}
