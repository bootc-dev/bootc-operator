// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/image"
)

// ImageMatchesDigest matches either the content digest or the manifest-list
// digest embedded in an ImageInfo's pullspec.
func ImageMatchesDigest(digest string) types.GomegaMatcher {
	return gomega.WithTransform(func(info *bootcv1alpha1.ImageInfo) bool {
		return image.InfoMatchesDigest(info, digest)
	}, gomega.BeTrue())
}
