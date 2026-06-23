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
	g := NewWithT(t)

	srv := httptest.NewServer(registry.New())
	defer srv.Close()

	// Build a minimal OCI image (empty base + one random layer) to push.
	layer, err := random.Layer(256, types.OCILayer)
	g.Expect(err).NotTo(HaveOccurred())
	img, err := mutate.AppendLayers(empty.Image, layer)
	g.Expect(err).NotTo(HaveOccurred())

	ref, err := name.ParseReference(fmt.Sprintf("%s/test/image:latest", srv.Listener.Addr().String()))
	g.Expect(err).NotTo(HaveOccurred())
	// Push the image to the in-memory registry so the resolver can find it.
	g.Expect(remote.Write(ref, img)).To(Succeed())

	want, err := img.Digest()
	g.Expect(err).NotTo(HaveOccurred())

	resolver := &GGCRResolver{}
	got, err := resolver.Resolve(context.Background(), ref.String())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(want.String()))
}

func TestResolveInvalidRef(t *testing.T) {
	g := NewWithT(t)

	resolver := &GGCRResolver{}
	_, err := resolver.Resolve(context.Background(), "")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("parsing reference"))
}

func TestResolveUnreachableRegistry(t *testing.T) {
	g := NewWithT(t)

	resolver := &GGCRResolver{}
	_, err := resolver.Resolve(context.Background(), "localhost:1/nonexistent/image:latest")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("fetching manifest"))
}
