/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

// Package reconciler implements interfaces that attempt to reconcile the
// desired state of the with the actual state of the world by triggering
// actions.
package reconciler

import (
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/controller/volume/attacherdetacher"
	"k8s.io/kubernetes/pkg/controller/volume/cache"
	"k8s.io/kubernetes/pkg/util/wait"
)

// Reconciler runs a periodic loop to reconcile the desired state of the with
// the actual state of the world by triggering attach detach operations.
type Reconciler interface {
	// Starts running the reconcilation loop which executes periodically, checks
	// if volumes that should be attached are attached and volumes that should
	// be detached are detached. If not, it will trigger attach/detach
	// operations to rectify.
	Run(stopCh <-chan struct{})
}

// NewReconciler returns a new instance of Reconciler that waits loopPeriod
// between successive executions.
// loopPeriod is the ammount of time the reconciler loop waits between
// successive executions.
// maxSafeToDetachDuration is the max ammount of time the reconciler will wait
// for the volume to deatch, after this it will detach the volume anyway
// assuming the node is unavilable. If during this time the volume becomes used
// by a new pod, the detach request will be aborted and the timer cleared.
func NewReconciler(
	loopPeriod time.Duration,
	maxSafeToDetachDuration time.Duration,
	desiredStateOfWorld cache.DesiredStateOfWorld,
	actualStateOfWorld cache.ActualStateOfWorld,
	attacherDetacher attacherdetacher.AttacherDetacher) Reconciler {
	return &reconciler{
		loopPeriod:              loopPeriod,
		maxSafeToDetachDuration: maxSafeToDetachDuration,
		desiredStateOfWorld:     desiredStateOfWorld,
		actualStateOfWorld:      actualStateOfWorld,
		attacherDetacher:        attacherDetacher,
	}
}

type reconciler struct {
	loopPeriod              time.Duration
	maxSafeToDetachDuration time.Duration
	desiredStateOfWorld     cache.DesiredStateOfWorld
	actualStateOfWorld      cache.ActualStateOfWorld
	attacherDetacher        attacherdetacher.AttacherDetacher
}

func (rc *reconciler) Run(stopCh <-chan struct{}) {
	wait.Until(rc.reconciliationLoopFunc(), rc.loopPeriod, stopCh)
}

func (rc *reconciler) reconciliationLoopFunc() func() {
	return func() {
		// Ensure volumes that should be attached are attached.
		for _, volumeToAttach := range rc.desiredStateOfWorld.GetVolumesToAttach() {
			if rc.actualStateOfWorld.VolumeNodeExists(
				volumeToAttach.VolumeName, volumeToAttach.NodeName) {
				// Volume/Node exists, touch it to reset "safe to detach"
				glog.V(12).Infof("Volume %q/Node %q is attached--touching.", volumeToAttach.VolumeName, volumeToAttach.NodeName)
				_, err := rc.actualStateOfWorld.AddVolumeNode(
					volumeToAttach.VolumeSpec, volumeToAttach.NodeName)
				if err != nil {
					glog.Errorf("Unexpected error on actualStateOfWorld.AddVolumeNode(): %v", err)
				}
			} else {
				// Volume/Node doesn't exist, spawn a goroutine to attach it
				glog.V(5).Infof("Triggering AttachVolume for volume %q to node %q", volumeToAttach.VolumeName, volumeToAttach.NodeName)
				rc.attacherDetacher.AttachVolume(&volumeToAttach, rc.actualStateOfWorld)
			}
		}

		// Ensure volumes that should be detached are detached.
		for _, attachedVolume := range rc.actualStateOfWorld.GetAttachedVolumes() {
			if !rc.desiredStateOfWorld.VolumeExists(
				attachedVolume.VolumeName, attachedVolume.NodeName) {
				// Volume exists in actual state of world but not desired
				if attachedVolume.SafeToDetach {
					glog.V(5).Infof("Triggering DetachVolume for volume %q to node %q", attachedVolume.VolumeName, attachedVolume.NodeName)
					rc.attacherDetacher.DetachVolume(&attachedVolume, rc.actualStateOfWorld)
				} else {
					// If volume is not safe to detach wait a max amount of time before detaching any way.
					timeElapsed, err := rc.actualStateOfWorld.MarkDesireToDetach(attachedVolume.VolumeName, attachedVolume.NodeName)
					if err != nil {
						glog.Errorf("Unexpected error actualStateOfWorld.MarkDesireToDetach(): %v", err)
					}
					if timeElapsed > rc.maxSafeToDetachDuration {
						glog.V(5).Infof("Triggering DetachVolume for volume %q to node %q. Volume is not safe to detach, but max wait time expired.", attachedVolume.VolumeName, attachedVolume.NodeName)
						rc.attacherDetacher.DetachVolume(&attachedVolume, rc.actualStateOfWorld)
					}
				}
			}
		}
	}
}
