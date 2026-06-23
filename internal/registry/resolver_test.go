// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	. "github.com/onsi/gomega"
)

func TestResolveValidTag(t *testing.T) {
	tags := []string{"latest", "v1.0", ""}

	g := NewWithT(t)

	srv := httptest.NewServer(registry.New())
	defer srv.Close()

	// Build a minimal OCI image (empty base + one random layer) to push.
	layer, err := random.Layer(256, types.OCILayer)
	g.Expect(err).NotTo(HaveOccurred())
	img, err := mutate.AppendLayers(empty.Image, layer)
	g.Expect(err).NotTo(HaveOccurred())

	for _, tag := range tags {
		t.Run(tag, func(t *testing.T) {
			var imageName string
			if tag == "" {
				imageName = fmt.Sprintf("%s/test/image", srv.Listener.Addr().String())
			} else {
				imageName = fmt.Sprintf("%s/test/image:%s", srv.Listener.Addr().String(), tag)
			}
			ref, err := name.ParseReference(imageName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(remote.Write(ref, img)).To(Succeed())

			want, err := img.Digest()
			g.Expect(err).NotTo(HaveOccurred())

			resolver := &GGCRResolver{}
			got, err := resolver.Resolve(context.Background(), ref.String())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(want.String()))
		})
	}
}

func TestResolveDigestRef(t *testing.T) {
	g := NewWithT(t)

	resolver := &GGCRResolver{}
	digest := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	ref := "registry.example.com/test/image@" + digest
	got, err := resolver.Resolve(context.Background(), ref)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(digest))
}

func TestResolveUnreachableRegistry(t *testing.T) {
	g := NewWithT(t)

	resolver := &GGCRResolver{}
	_, err := resolver.Resolve(context.Background(), "localhost:1/nonexistent/image:latest")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("fetching manifest"))
}
