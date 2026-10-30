// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

func TestImageMatchesDigest(t *testing.T) {
	contentDigest := "sha256:" + strings.Repeat("a", 64)
	listDigest := "sha256:" + strings.Repeat("b", 64)
	info := &bootcv1alpha1.ImageInfo{
		Image:       "example.com/node@" + listDigest,
		ImageDigest: contentDigest,
	}

	for _, tt := range []struct {
		name   string
		info   *bootcv1alpha1.ImageInfo
		digest string
		want   bool
	}{
		{"content digest", info, contentDigest, true},
		{"manifest-list digest", info, listDigest, true},
		{"different digest", info, "sha256:" + strings.Repeat("c", 64), false},
		{"missing image", nil, listDigest, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			matched, err := ImageMatchesDigest(tt.digest).Match(tt.info)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(matched).To(Equal(tt.want))
		})
	}
}
