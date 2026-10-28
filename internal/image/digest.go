// SPDX-License-Identifier: Apache-2.0

// Package image provides helpers for matching container image
// references against digests.
package image

import (
	"github.com/distribution/reference"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// InfoMatchesDigest reports whether the given ImageInfo corresponds
// to the supplied digest. It checks both the resolved content digest
// (ImageDigest) and the pullspec digest embedded in the Image reference.
// The latter handles manifest-list digests that differ from the
// platform-specific content digest bootc resolves at pull time.
//
// For example, on EKS `bootc status` shows:
//
//	Image: registry.stage.redhat.io/rhel10/rhel-bootc-eks@sha256:ff63b...
//	Digest: sha256:db1b41... (amd64)
//
// The Image field carries the manifest-list digest (ff63b...) while
// the Digest field carries the platform-specific content digest
// (db1b41...). Both must be accepted as a match.
func InfoMatchesDigest(info *bootcv1alpha1.ImageInfo, digest string) bool {
	if info == nil {
		return false
	}
	if info.ImageDigest == digest {
		return true
	}
	ref, err := reference.ParseNamed(info.Image)
	if err != nil {
		return false
	}
	digested, ok := ref.(reference.Digested)
	if !ok {
		return false
	}
	return digested.Digest().String() == digest
}
