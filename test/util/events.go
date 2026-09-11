// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ShortDigest abbreviates a "sha256:<hex>" digest the same way the controller
// does when rendering event notes ("sha256:" plus 12 hex characters).
func ShortDigest(digest string) string {
	const shortLen = len("sha256:") + 12
	if len(digest) > shortLen {
		return digest[:shortLen]
	}
	return digest
}

// FilterEventsByObject returns events regarding the identified object.
func FilterEventsByObject(
	items []eventsv1.Event,
	kind, name string,
	uid types.UID,
) []eventsv1.Event {
	events := make([]eventsv1.Event, 0, len(items))
	for _, event := range items {
		if event.Regarding.Kind == kind &&
			event.Regarding.Name == name &&
			event.Regarding.UID == uid {
			events = append(events, event)
		}
	}
	return events
}
