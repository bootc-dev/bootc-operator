// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
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
			got, err := resolver.Resolve(context.Background(), ref.String(), nil)
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
	got, err := resolver.Resolve(context.Background(), ref, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(digest))
}

func TestResolveUnreachableRegistry(t *testing.T) {
	g := NewWithT(t)

	resolver := &GGCRResolver{}
	_, err := resolver.Resolve(context.Background(), "localhost:1/nonexistent/image:latest", nil)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("fetching manifest"))
}

func TestDockerConfigResolve(t *testing.T) {
	tests := []struct {
		name     string
		auths    []string
		ref      string
		wantUser string
	}{
		{"exact host", []string{"quay.io"}, "quay.io/org/img:v1", "quay.io"},
		{
			"host with port",
			[]string{"reg.example.com:5000"},
			"reg.example.com:5000/img",
			"reg.example.com:5000",
		},
		{"https scheme", []string{"https://quay.io"}, "quay.io/org/img", "https://quay.io"},
		{"http scheme", []string{"http://quay.io"}, "quay.io/org/img", "http://quay.io"},
		{
			"scheme and path",
			[]string{"https://quay.io/v2/"},
			"quay.io/org/img",
			"https://quay.io/v2/",
		},
		{
			"hub v1 url",
			[]string{"https://index.docker.io/v1/"},
			"docker.io/library/busybox",
			"https://index.docker.io/v1/",
		},
		{
			"hub v1 url, implicit registry",
			[]string{"https://index.docker.io/v1/"},
			"busybox",
			"https://index.docker.io/v1/",
		},
		{
			"hub v1 url, registry-1 ref",
			[]string{"https://index.docker.io/v1/"},
			"registry-1.docker.io/library/busybox",
			"https://index.docker.io/v1/",
		},
		{"hub docker.io key", []string{"docker.io"}, "busybox", "docker.io"},
		{"hub registry-1 key", []string{"registry-1.docker.io"}, "busybox", "registry-1.docker.io"},
		{
			"hub registry-1 url",
			[]string{"https://registry-1.docker.io/v2/"},
			"busybox",
			"https://registry-1.docker.io/v2/",
		},
		{
			"exact wins over normalized",
			[]string{"https://quay.io/v2/", "quay.io"},
			"quay.io/img",
			"quay.io",
		},
		{
			"normalized ties go to the first sorted key",
			[]string{"http://quay.io", "https://quay.io/a"},
			"quay.io/img",
			"http://quay.io",
		},
		{"other registry", []string{"https://index.docker.io/v1/"}, "quay.io/org/img", ""},
		{"path without scheme is not a host", []string{"quay.io/org"}, "quay.io/org/img", ""},
		{"no entries", nil, "quay.io/org/img", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			cfg := &dockerConfigJSON{Auths: map[string]authn.AuthConfig{}}
			for _, key := range tt.auths {
				cfg.Auths[key] = authn.AuthConfig{Username: key, Password: "pw-" + key}
			}
			ref, err := name.ParseReference(tt.ref)
			g.Expect(err).NotTo(HaveOccurred())

			a, err := cfg.Resolve(ref.Context())
			g.Expect(err).NotTo(HaveOccurred())
			got, err := a.Authorization()
			g.Expect(err).NotTo(HaveOccurred())
			if tt.wantUser == "" {
				g.Expect(*got).To(Equal(authn.AuthConfig{}))
				return
			}
			g.Expect(*got).To(Equal(authn.AuthConfig{
				Username: tt.wantUser,
				Password: "pw-" + tt.wantUser,
			}))
		})
	}
}

func TestKeychainFromDockerConfigHubURL(t *testing.T) {
	g := NewWithT(t)

	// The entry kubectl create secret docker-registry writes by default.
	kc, err := keychainFromDockerConfig([]byte(
		`{"auths":{"https://index.docker.io/v1/":{"username":"u","password":"p"}}}`))
	g.Expect(err).NotTo(HaveOccurred())
	ref, err := name.ParseReference("docker.io/library/busybox")
	g.Expect(err).NotTo(HaveOccurred())

	a, err := kc.Resolve(ref.Context())
	g.Expect(err).NotTo(HaveOccurred())
	got, err := a.Authorization()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.Username).To(Equal("u"))
	g.Expect(got.Password).To(Equal("p"))
}
