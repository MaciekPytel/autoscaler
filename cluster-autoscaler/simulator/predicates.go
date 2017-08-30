/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package simulator

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	apiv1 "k8s.io/api/core/v1"
	kube_util "k8s.io/autoscaler/cluster-autoscaler/utils/kubernetes"
	informers "k8s.io/client-go/informers"
	kube_client "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm/predicates"
	"k8s.io/kubernetes/plugin/pkg/scheduler/factory"
	"k8s.io/kubernetes/plugin/pkg/scheduler/schedulercache"

	// We need to import provider to intialize default scheduler.
	_ "k8s.io/kubernetes/plugin/pkg/scheduler/algorithmprovider"

	"github.com/golang/glog"
)

// ErrorVerbosity defines the verbosity of error messages returned by PredicateChecker
type ErrorVerbosity bool

const (
	// ReturnVerboseError causes informative error messages to be returned.
	ReturnVerboseError ErrorVerbosity = true
	// ReturnSimpleError causes simple, detail-free error messages to be returned.
	// This significantly improves performance and is useful if the error message
	// is discarded anyway.
	ReturnSimpleError ErrorVerbosity = false

	// We want to disable affinity predicate for performance reasons if no ppod
	// requires it
	affinityPredicateName = "MatchInterPodAffinity"
)

type predicateInfo struct {
	name      string
	predicate algorithm.FitPredicate
}

type predicateStatistics struct {
	callCount     int64
	totalDuration time.Duration
}

// PredicateChecker checks whether all required predicates are matched for given Pod and Node
type PredicateChecker struct {
	predicates []predicateInfo
	predicateStats map[string]*predicateStatistics
	predicateMetadataProducer algorithm.MetadataProducer
	enableAffinityPredicate   bool
}

// there are no const arrays in go, this is meant to be used as a const
var priorityPredicates = []string{"PodFitsResources", "GeneralPredicates", "PodToleratesNodeTaints"}

// NewPredicateChecker builds PredicateChecker.
func NewPredicateChecker(kubeClient kube_client.Interface, stop <-chan struct{}) (*PredicateChecker, error) {
	provider, err := factory.GetAlgorithmProvider(factory.DefaultProvider)
	if err != nil {
		return nil, err
	}
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)

	schedulerConfigFactory := factory.NewConfigFactory(
		"cluster-autoscaler",
		kubeClient,
		informerFactory.Core().V1().Nodes(),
		informerFactory.Core().V1().Pods(),
		informerFactory.Core().V1().PersistentVolumes(),
		informerFactory.Core().V1().PersistentVolumeClaims(),
		informerFactory.Core().V1().ReplicationControllers(),
		informerFactory.Extensions().V1beta1().ReplicaSets(),
		informerFactory.Apps().V1beta1().StatefulSets(),
		informerFactory.Core().V1().Services(),
		apiv1.DefaultHardPodAffinitySymmetricWeight,
		false,
	)

	informerFactory.Start(stop)

	metadataProducer, err := schedulerConfigFactory.GetPredicateMetadataProducer()
	if err != nil {
		return nil, err
	}
	predicateMap, err := schedulerConfigFactory.GetPredicates(provider.FitPredicateKeys)
	predicateMap["ready"] = isNodeReadyAndSchedulablePredicate
	if err != nil {
		return nil, err
	}
	// We always want to have PodFitsResources as a first predicate we run
	// as this is cheap to check and it should be enough to fail predicates
	// in most of our simulations (especially binpacking).
	if _, found := predicateMap["PodFitsResources"]; !found {
		predicateMap["PodFitsResources"] = predicates.PodFitsResources
	}

	predicateList := make([]predicateInfo, 0)
	for _, predicateName := range priorityPredicates {
		if predicate, found := predicateMap[predicateName]; found {
			predicateList = append(predicateList, predicateInfo{name: predicateName, predicate: predicate})
			delete(predicateMap, predicateName)
		}
	}
	for predicateName, predicate := range predicateMap {
		predicateList = append(predicateList, predicateInfo{name: predicateName, predicate: predicate})
	}

	predStats := make(map[string]*predicateStatistics, 0)
	for _, predInfo := range predicateList {
		predStats[predInfo.name] = &predicateStatistics{callCount: 0, totalDuration: time.Duration(0)}
		glog.V(1).Infof("Using predicate %s", predInfo.name)
	}
	predStats["__precompute"] = &predicateStatistics{callCount: 0, totalDuration: time.Duration(0)}

	// TODO: Verify that run is not needed anymore.
	// schedulerConfigFactory.Run()

	return &PredicateChecker{
		predicates: predicateList,
		predicateStats: predStats,
		predicateMetadataProducer: metadataProducer,
		enableAffinityPredicate:   true,
	}, nil
}

func isNodeReadyAndSchedulablePredicate(pod *apiv1.Pod, meta interface{}, nodeInfo *schedulercache.NodeInfo) (bool,
	[]algorithm.PredicateFailureReason, error) {
	ready := kube_util.IsNodeReadyAndSchedulable(nodeInfo.Node())
	if !ready {
		return false, []algorithm.PredicateFailureReason{predicates.NewFailureReason("node is unready")}, nil
	}
	return true, []algorithm.PredicateFailureReason{}, nil
}

// NewTestPredicateChecker builds test version of PredicateChecker.
func NewTestPredicateChecker() *PredicateChecker {
	return &PredicateChecker{
		predicates: []predicateInfo{
			{name: "default", predicate: predicates.GeneralPredicates},
			{name: "ready", predicate: isNodeReadyAndSchedulablePredicate},
		},
		predicateStats: map[string]*predicateStatistics{
			"default": &predicateStatistics{callCount: 0, totalDuration: time.Duration(0)},
			"ready":   &predicateStatistics{callCount: 0, totalDuration: time.Duration(0)},
		},
		predicateMetadataProducer: func(_ *apiv1.Pod, _ map[string]*schedulercache.NodeInfo) interface{} {
			return nil
		},
	}
}

func (p *PredicateChecker) GetPredicateMetadata(pod *apiv1.Pod, nodeInfos map[string]*schedulercache.NodeInfo) interface{} {
	if !p.enableAffinityPredicate {
		return nil
	}
	start := time.Now()
	result := p.predicateMetadataProducer(pod, nodeInfos)
	p.predicateStats["__precompute"].callCount += 1
	p.predicateStats["__precompute"].totalDuration += time.Now().Sub(start)
	return result
}

// SetAffinityPredicateEnabled can be used to enable or disable checking MatchInterPodAffinity
// predicate. This will cause incorrect CA behavior if there is at least a single pod in
// cluster using affinity/antiaffinity. However, checking affinity predicate is extremly
// costly even if no pod is using it, so it may be worth disabling it in such situation.
func (p *PredicateChecker) SetAffinityPredicateEnabled(enable bool) {
	p.enableAffinityPredicate = enable
}

// FitsAny checks if the given pod can be place on any of the given nodes.
func (p *PredicateChecker) FitsAny(pod *apiv1.Pod, nodeInfos map[string]*schedulercache.NodeInfo) (string, error) {
	for name, nodeInfo := range nodeInfos {
		// Be sure that the node is schedulable.
		if nodeInfo.Node().Spec.Unschedulable {
			continue
		}
		if err := p.CheckPredicates(pod, nil, nodeInfo, ReturnSimpleError); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("cannot put pod %s on any node", pod.Name)
}

// CheckPredicates checks if the given pod can be placed on the given node.
// We're running a ton of predicates and more often than not we only care whether
// they pass or not and don't care for a reason. Turns out formatting nice error
// messages gets very expensive, so we only do it if verbose is set to ReturnVerboseError.
func (p *PredicateChecker) CheckPredicates(pod *apiv1.Pod, predicateMetadata interface{}, nodeInfo *schedulercache.NodeInfo, verbosity ErrorVerbosity) error {
	for _, predInfo := range p.predicates {
		// skip affinity predicate if it has been disabled
		if !p.enableAffinityPredicate && predInfo.name == affinityPredicateName {
			continue
		}

		start := time.Now()
		match, failureReason, err := predInfo.predicate(pod, predicateMetadata, nodeInfo)
		p.predicateStats[predInfo.name].callCount += 1
		p.predicateStats[predInfo.name].totalDuration += time.Now().Sub(start)

		if verbosity == ReturnSimpleError && (err != nil || !match) {
			return errors.New("Predicates failed")
		}

		nodename := "unknown"
		if nodeInfo.Node() != nil {
			nodename = nodeInfo.Node().Name
		}
		if err != nil {
			return fmt.Errorf("%s predicate error, cannot put %s/%s on %s due to, error %v", predInfo.name, pod.Namespace,
				pod.Name, nodename, err)
		}
		if !match {
			var buffer bytes.Buffer
			for i, reason := range failureReason {
				if i > 0 {
					buffer.WriteString(",")
				}
				buffer.WriteString(reason.GetReason())
			}
			return fmt.Errorf("%s predicate mismatch, cannot put %s/%s on %s, reason: %s", predInfo.name, pod.Namespace,
				pod.Name, nodename, buffer.String())
		}
	}
	return nil
}

func (p *PredicateChecker) LogPredicateStats() {
	for name, stats := range p.predicateStats {
		if stats.callCount > 0 {
			glog.Errorf("Predicate statistics for %s: called %v times, total time %v, mean duration %v", name, stats.callCount, stats.totalDuration, stats.totalDuration / time.Duration(stats.callCount))
		}
	}
}
