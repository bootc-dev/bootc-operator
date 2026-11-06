// SPDX-License-Identifier: Apache-2.0

package pullsecret

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

func TestKey(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		secret    string
		want      string
		wantErr   bool
	}{
		{
			"namespace and name",
			"bootc-operator",
			"my-pull-secret",
			"bootc-operator_my-pull-secret",
			false,
		},
		{"dotted name", "team-a", "registry.example.com", "team-a_registry.example.com", false},
		{"at limit", "ns", strings.Repeat("a", 250), "ns_" + strings.Repeat("a", 250), false},
		{"over limit", "ns", strings.Repeat("a", 251), "", true},
		{"long namespace and name", strings.Repeat("n", 63), strings.Repeat("a", 253), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			got, err := Key(tt.namespace, tt.secret)
			if tt.wantErr {
				g.Expect(err).To(MatchError(ErrKeyTooLong))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestKeyRejectsInvalidNames(t *testing.T) {
	for _, tt := range []struct{ namespace, name string }{
		{"", "secret"},
		{"ns", ""},
		{"..", "secret"},
		{"ns", "../../etc/shadow"},
		{"ns", "a/b"},
		{"ns", "a_b"},
		{"Team-A", "secret"},
	} {
		t.Run(tt.namespace+"/"+tt.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := Key(tt.namespace, tt.name)
			g.Expect(err).To(MatchError(ContainSubstring("invalid")))
		})
	}
}

func TestAuthData(t *testing.T) {
	const (
		current = `{"auths":{"quay.io":{"auth":"Zm9vOmJhcg=="}}}`
		legacy  = `{"quay.io":{"auth":"Zm9vOmJhcg==","identitytoken":"tok"}}`
	)
	tests := []struct {
		name    string
		data    map[string]string
		want    string
		wantErr string
	}{
		{
			name: "dockerconfigjson as is",
			data: map[string]string{corev1.DockerConfigJsonKey: current},
			want: current,
		},
		{
			name: "dockerconfigjson is not rewrapped",
			data: map[string]string{corev1.DockerConfigJsonKey: legacy},
			want: legacy,
		},
		{
			name: "legacy dockercfg wrapped, entries untouched",
			data: map[string]string{corev1.DockerConfigKey: legacy},
			want: `{"auths":` + legacy + `}`,
		},
		{
			name: "legacy dockercfg empty",
			data: map[string]string{corev1.DockerConfigKey: `{}`},
			want: `{"auths":{}}`,
		},
		{
			name: "dockerconfigjson preferred when both set",
			data: map[string]string{
				corev1.DockerConfigJsonKey: current,
				corev1.DockerConfigKey:     legacy,
			},
			want: current,
		},
		{
			name: "malformed dockerconfigjson is not replaced by dockercfg",
			data: map[string]string{
				corev1.DockerConfigJsonKey: `{"auths":`,
				corev1.DockerConfigKey:     legacy,
			},
			wantErr: corev1.DockerConfigJsonKey + " is not a JSON object",
		},
		{
			name:    "malformed dockerconfigjson",
			data:    map[string]string{corev1.DockerConfigJsonKey: `{"auths":`},
			wantErr: corev1.DockerConfigJsonKey + " is not a JSON object",
		},
		{
			name:    "dockerconfigjson not an object",
			data:    map[string]string{corev1.DockerConfigJsonKey: `["a"]`},
			wantErr: corev1.DockerConfigJsonKey + " is not a JSON object",
		},
		{
			name:    "dockerconfigjson null",
			data:    map[string]string{corev1.DockerConfigJsonKey: `null`},
			wantErr: corev1.DockerConfigJsonKey + " is not a JSON object",
		},
		{
			name:    "malformed dockercfg",
			data:    map[string]string{corev1.DockerConfigKey: `secret-token`},
			wantErr: corev1.DockerConfigKey + " is not a JSON object",
		},
		{
			name:    "neither key",
			data:    map[string]string{"token": "x"},
			wantErr: "missing " + corev1.DockerConfigJsonKey + " or " + corev1.DockerConfigKey,
		},
		{
			name:    "no data",
			wantErr: "missing " + corev1.DockerConfigJsonKey + " or " + corev1.DockerConfigKey,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			secret := &corev1.Secret{Data: map[string][]byte{}}
			for k, v := range tt.data {
				secret.Data[k] = []byte(v)
			}
			got, err := AuthData(secret)
			if tt.wantErr != "" {
				g.Expect(err).To(MatchError(ContainSubstring(tt.wantErr)))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(got)).To(MatchJSON(tt.want))
		})
	}
}

func TestAuthDataKeepsLegacyBytes(t *testing.T) {
	g := NewWithT(t)
	// Characters that a JSON re-encoder would escape or strip.
	legacy := `{ "quay.io" : {"auth":"a&b<c>"} }`
	secret := &corev1.Secret{Data: map[string][]byte{
		corev1.DockerConfigKey: []byte(legacy),
	}}
	got, err := AuthData(secret)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(got)).To(Equal(`{"auths":` + legacy + `}`))
}

func TestAuthDataErrorOmitsSecretData(t *testing.T) {
	g := NewWithT(t)
	secret := &corev1.Secret{Data: map[string][]byte{
		corev1.DockerConfigKey: []byte("hunter2-not-json"),
	}}
	_, err := AuthData(secret)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).NotTo(ContainSubstring("hunter2"))
}
