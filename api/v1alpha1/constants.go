/*
Copyright 2026.

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

package v1alpha1

// Well-known labels and annotations applied to Nodes by the controller.
const (
	// LabelManaged is set on Nodes that are managed by a BootcNodePool.
	// Its presence triggers the DaemonSet to schedule a daemon pod on
	// the node.
	LabelManaged = "bootc.dev/managed"

	// AnnotationWasCordoned records whether a node was already cordoned
	// before the controller cordoned it for a reboot. Used to restore
	// prior cordon state after update.
	AnnotationWasCordoned = "bootc.dev/was-cordoned"
)
