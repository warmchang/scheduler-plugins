/*
Copyright 2020 The Kubernetes Authors.

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

package noderesources

import (
	"context"
	"fmt"
	"math"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	schedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"sigs.k8s.io/scheduler-plugins/apis/config"
	"sigs.k8s.io/scheduler-plugins/apis/config/validation"
)

// Allocatable is a score plugin that favors nodes based on their allocatable
// resources.
type Allocatable struct {
	logger klog.Logger
	handle framework.Handle
	resourceAllocationScorer
}

var _ = framework.ScorePlugin(&Allocatable{})

// AllocatableName is the name of the plugin used in the Registry and configurations.
const AllocatableName = "NodeResourcesAllocatable"

// Name returns name of the plugin. It is used in logs, etc.
func (alloc *Allocatable) Name() string {
	return AllocatableName
}

func validateResources(resources []schedulerconfig.ResourceSpec) error {
	for _, resource := range resources {
		if resource.Weight <= 0 {
			return fmt.Errorf("resource Weight of %v should be a positive value, got %v", resource.Name, resource.Weight)
		}
		// No upper bound on weight.
	}
	return nil
}

// Score invoked at the score extension point.
func (alloc *Allocatable) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	logger := klog.FromContext(klog.NewContext(ctx, alloc.logger)).WithValues("ExtensionPoint", "Score")
	nodeInfo, err := alloc.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	// alloc.score favors nodes with least allocatable or most allocatable resources.
	// It calculates the sum of the node's weighted allocatable resources.
	//
	// Note: the returned "score" is negative for least allocatable, and positive for most allocatable.
	return alloc.score(logger, pod, nodeInfo)
}

// ScoreExtensions of the Score plugin.
func (alloc *Allocatable) ScoreExtensions() framework.ScoreExtensions {
	return alloc
}

// NewAllocatable initializes a new plugin and returns it.
func NewAllocatable(ctx context.Context, allocArgs runtime.Object, h framework.Handle) (framework.Plugin, error) {
	logger := klog.FromContext(ctx).WithValues("plugin", AllocatableName)
	// Start with default values.
	var mode config.ModeType
	resToWeightMap := defaultResourcesToWeightMap

	// Update values from args, if specified.
	if allocArgs != nil {
		args, ok := allocArgs.(*config.NodeResourcesAllocatableArgs)
		if !ok {
			return nil, fmt.Errorf("want args to be of type NodeResourcesAllocatableArgs, got %T", allocArgs)
		}
		if args.Mode == "" {
			args.Mode = config.Least
		}
		if err := validation.ValidateNodeResourcesAllocatableArgs(args, nil); err != nil {
			return nil, err
		}
		if len(args.Resources) > 0 {
			resToWeightMap = make(resourceToWeightMap)
			for _, resource := range args.Resources {
				resToWeightMap[v1.ResourceName(resource.Name)] = resource.Weight
			}
		}
		mode = args.Mode
	}

	return &Allocatable{
		logger: logger,
		handle: h,
		resourceAllocationScorer: resourceAllocationScorer{
			Name:                AllocatableName,
			scorer:              resourceScorer(logger, resToWeightMap, mode),
			resourceToWeightMap: resToWeightMap,
		},
	}, nil
}

func resourceScorer(logger klog.Logger, resToWeightMap resourceToWeightMap, mode config.ModeType) func(resourceToValueMap, resourceToValueMap) int64 {
	return func(requested, allocable resourceToValueMap) int64 {
		// TODO: consider volumes in scoring.
		var nodeScore, weightSum int64
		for resource, weight := range resToWeightMap {
			resourceScore := score(logger, allocable[resource], mode)
			nodeScore += resourceScore * weight
			weightSum += weight
		}
		return nodeScore / weightSum
	}
}

func score(logger klog.Logger, capacity int64, mode config.ModeType) int64 {
	switch mode {
	case config.Least:
		return -1 * capacity
	case config.Most:
		return capacity
	}

	logger.V(10).Info("No match for mode", "mode", mode)
	return 0
}

// NormalizeScore invoked after scoring all nodes.
func (alloc *Allocatable) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	// Find highest and lowest scores.
	var highest int64 = -math.MaxInt64
	var lowest int64 = math.MaxInt64
	for _, nodeScore := range scores {
		if nodeScore.Score > highest {
			highest = nodeScore.Score
		}
		if nodeScore.Score < lowest {
			lowest = nodeScore.Score
		}
	}

	// Transform the highest to lowest score range to fit the framework's min to max node score range.
	oldRange := highest - lowest
	newRange := framework.MaxNodeScore - framework.MinNodeScore
	for i, nodeScore := range scores {
		if oldRange == 0 {
			scores[i].Score = framework.MinNodeScore
		} else {
			scores[i].Score = ((nodeScore.Score - lowest) * newRange / oldRange) + framework.MinNodeScore
		}
	}

	return nil
}
